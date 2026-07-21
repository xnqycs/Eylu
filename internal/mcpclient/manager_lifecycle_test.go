package mcpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"Eylu/internal/config"
)

func TestManagerDisableDuringConnectCannotReviveServer(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server {
		return newPagedProtocolServer()
	}, &sdkmcp.StreamableHTTPOptions{JSONResponse: true})
	httpServer := httptest.NewServer(blockFirstRequest(handler, started, release))
	t.Cleanup(httpServer.Close)

	manager := newLifecycleTestManager(t, config.MCPServerConfig{
		Transport: config.MCPTransportStreamableHTTP,
		URL:       httpServer.URL,
	})
	connectResult := make(chan error, 1)
	go func() { connectResult <- manager.connect(context.Background(), "remote", StatusConnecting) }()
	waitForSignal(t, started, "connection attempt")

	if err := manager.Disable(context.Background(), "remote"); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-connectResult:
	case <-time.After(3 * time.Second):
		t.Fatal("connect did not stop after disable")
	}

	runtime := manager.servers["remote"]
	runtime.mu.RLock()
	status, session, enabled := runtime.status, runtime.session, runtime.config.IsEnabled()
	runtimeTools := len(runtime.tools)
	runtime.mu.RUnlock()
	if status != StatusDisabled || session != nil || enabled {
		t.Fatalf("disabled server was revived: status=%s session=%p enabled=%v", status, session, enabled)
	}
	if managerTools := len(manager.Tools()); runtimeTools != 0 || managerTools != 0 {
		t.Fatalf("disabled server repopulated its catalog: runtime=%d manager=%d", runtimeTools, managerTools)
	}
}

func TestManagerCloseDuringConnectCannotInstallSession(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server {
		return newPagedProtocolServer()
	}, &sdkmcp.StreamableHTTPOptions{JSONResponse: true})
	httpServer := httptest.NewServer(blockFirstRequest(handler, started, release))
	t.Cleanup(httpServer.Close)

	manager := newLifecycleTestManager(t, config.MCPServerConfig{
		Transport: config.MCPTransportStreamableHTTP,
		URL:       httpServer.URL,
	})
	connectResult := make(chan error, 1)
	go func() { connectResult <- manager.connect(context.Background(), "remote", StatusConnecting) }()
	waitForSignal(t, started, "connection attempt")

	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-connectResult:
	case <-time.After(3 * time.Second):
		t.Fatal("connect did not stop after manager close")
	}

	runtime := manager.servers["remote"]
	runtime.mu.RLock()
	status, session := runtime.status, runtime.session
	runtimeTools := len(runtime.tools)
	runtime.mu.RUnlock()
	if status != StatusDisconnected || session != nil {
		t.Fatalf("closed manager installed a session: status=%s session=%p", status, session)
	}
	if managerTools := len(manager.Tools()); runtimeTools != 0 || managerTools != 0 {
		t.Fatalf("closed manager repopulated its catalog: runtime=%d manager=%d", runtimeTools, managerTools)
	}
}

func TestManagerEventSubscriptionConcurrentCloseIsSafe(t *testing.T) {
	manager := newLifecycleTestManager(t, config.MCPServerConfig{Disabled: true})
	var workers sync.WaitGroup
	start := make(chan struct{})
	for worker := 0; worker < 16; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for index := 0; index < 500; index++ {
				events, unsubscribe := manager.SubscribeEvents(1)
				manager.publish(Event{Kind: EventStatus, Server: "remote", Status: StatusDisconnected})
				unsubscribe()
				for range events {
				}
			}
		}()
	}
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for index := 0; index < 2000; index++ {
				manager.publish(Event{Kind: EventStatus, Server: "remote", Status: StatusDisconnected})
			}
		}()
	}
	close(start)
	time.Sleep(5 * time.Millisecond)
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
}

func TestManagerNeedsAuthStopsReconnectUntilExplicitReconnect(t *testing.T) {
	var unauthorized atomic.Bool
	unauthorized.Store(true)
	var requests atomic.Int64
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server {
		return newPagedProtocolServer()
	}, &sdkmcp.StreamableHTTPOptions{JSONResponse: true})
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if unauthorized.Load() {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(httpServer.Close)

	manager := newLifecycleTestManager(t, config.MCPServerConfig{
		Transport: config.MCPTransportStreamableHTTP,
		URL:       httpServer.URL,
	})
	if err := manager.connect(context.Background(), "remote", StatusConnecting); err == nil {
		t.Fatal("unauthorized connection unexpectedly succeeded")
	}
	runtime := manager.servers["remote"]
	runtime.mu.RLock()
	status := runtime.status
	runtime.mu.RUnlock()
	if status != StatusNeedsAuth {
		t.Fatalf("status=%s, want %s", status, StatusNeedsAuth)
	}
	before := requests.Load()
	time.Sleep(50 * time.Millisecond)
	if after := requests.Load(); after != before {
		t.Fatalf("needs_auth triggered an automatic retry: before=%d after=%d", before, after)
	}

	unauthorized.Store(false)
	if err := manager.Reconnect(context.Background(), "remote"); err != nil {
		t.Fatal(err)
	}
	if detail, err := manager.Inspect("remote"); err != nil || detail.Status != StatusConnected {
		t.Fatalf("explicit reconnect did not recover: detail=%#v error=%v", detail, err)
	}
}

func TestManagerInitialBadGatewayWaitsForExplicitReconnect(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	var requests atomic.Int64
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server {
		return newPagedProtocolServer()
	}, &sdkmcp.StreamableHTTPOptions{JSONResponse: true})
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if failing.Load() {
			http.Error(writer, "temporary failure", http.StatusBadGateway)
			return
		}
		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(httpServer.Close)

	manager, diagnostics, err := Open(context.Background(), map[string]config.MCPServerConfig{
		"remote": {Transport: config.MCPTransportStreamableHTTP, URL: httpServer.URL},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0].Message, "Bad Gateway") {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
	before := requests.Load()
	time.Sleep(250 * time.Millisecond)
	detail, inspectErr := manager.Inspect("remote")
	if inspectErr != nil || detail.Status != StatusError || requests.Load() != before {
		t.Fatalf("failure retried automatically: detail=%#v before=%d after=%d error=%v", detail, before, requests.Load(), inspectErr)
	}

	failing.Store(false)
	if err := manager.Reconnect(context.Background(), "remote"); err != nil {
		t.Fatal(err)
	}
	if detail, err := manager.Inspect("remote"); err != nil || detail.Status != StatusConnected {
		t.Fatalf("explicit reconnect did not recover: detail=%#v error=%v", detail, err)
	}
}

func newLifecycleTestManager(t *testing.T, cfg config.MCPServerConfig) *Manager {
	t.Helper()
	managerContext, cancel := context.WithCancel(context.Background())
	manager := &Manager{
		ctx:         managerContext,
		cancel:      cancel,
		workspace:   t.TempDir(),
		servers:     make(map[string]*serverRuntime),
		subscribers: make(map[uint64]chan Event),
	}
	status := StatusDisconnected
	if !cfg.IsEnabled() {
		status = StatusDisabled
	}
	manager.servers["remote"] = &serverRuntime{
		manager:  manager,
		name:     "remote",
		config:   cfg,
		status:   status,
		readOnly: make(map[string]bool),
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func blockFirstRequest(next http.Handler, started chan<- struct{}, release <-chan struct{}) http.Handler {
	var once sync.Once
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		blocked := false
		once.Do(func() {
			blocked = true
			close(started)
		})
		if blocked {
			<-release
		}
		next.ServeHTTP(writer, request)
	})
}

func waitForSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}
