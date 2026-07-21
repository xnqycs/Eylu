package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"Eylu/internal/config"
)

func TestManagerLegacySSEEndToEnd(t *testing.T) {
	server := newPagedProtocolServer()
	handler := newLegacyOnlySSEHandler(sdkmcp.NewSSEHandler(func(*http.Request) *sdkmcp.Server { return server }, nil))
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	manager, diagnostics, err := Open(context.Background(), map[string]config.MCPServerConfig{
		"legacy": {Transport: config.MCPTransportSSE, URL: httpServer.URL, StartupTimeoutSeconds: 5},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
	detail, err := manager.Inspect("legacy")
	if err != nil || detail.Status != StatusConnected || detail.ServerInfo.Tools != 2 || len(detail.Resources) != 2 || len(detail.ResourceTemplates) != 2 || len(detail.Prompts) != 2 {
		t.Fatalf("detail=%#v error=%v", detail, err)
	}
	result, err := manager.ReadResource(context.Background(), "legacy", "fixture://two")
	if err != nil || result.Contents[0].Text != "fixture://two" {
		t.Fatalf("resource=%#v error=%v", result, err)
	}
}

func TestManagerLegacySSEResumesWithLastEventID(t *testing.T) {
	fixture := newResumableLegacySSEFixture()
	httpServer := httptest.NewServer(fixture)
	defer httpServer.Close()
	fixture.endpoint = httpServer.URL + "/messages"

	manager, diagnostics, err := Open(context.Background(), map[string]config.MCPServerConfig{
		"legacy": {Transport: config.MCPTransportSSE, URL: httpServer.URL, StartupTimeoutSeconds: 5},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if len(diagnostics) != 0 {
		fixture.mu.Lock()
		lastEventID, pending := fixture.lastEventID, fixture.pending
		fixture.mu.Unlock()
		t.Fatalf("diagnostics=%#v Last-Event-ID=%q pending=%t", diagnostics, lastEventID, pending != "")
	}
	select {
	case <-fixture.resumed:
	default:
		t.Fatal("legacy SSE stream was not resumed")
	}
	fixture.mu.Lock()
	lastEventID, expectedLastEventID := fixture.lastEventID, fixture.expectedLastEventID
	fixture.mu.Unlock()
	if lastEventID != expectedLastEventID {
		t.Fatalf("Last-Event-ID = %q, want %q", lastEventID, expectedLastEventID)
	}
	detail, err := manager.Inspect("legacy")
	if err != nil || detail.Status != StatusConnected || detail.ServerInfo.Tools != 1 || detail.Tools[0].Name != "resumed" {
		t.Fatalf("detail=%#v error=%v", detail, err)
	}
}

type resumableLegacySSEFixture struct {
	mu                  sync.Mutex
	endpoint            string
	stream              *legacyResumeStream
	pending             string
	nextEventID         int
	dropped             bool
	lastEventID         string
	expectedLastEventID string
	resumed             chan struct{}
	resumeOnce          sync.Once
}

type legacyResumeStream struct {
	events     chan string
	disconnect chan struct{}
	once       sync.Once
}

func newResumableLegacySSEFixture() *resumableLegacySSEFixture {
	return &resumableLegacySSEFixture{resumed: make(chan struct{})}
}

func (h *resumableLegacySSEFixture) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet {
		h.serveStream(writer, request)
		return
	}
	if request.Method != http.MethodPost || request.URL.Path != "/messages" {
		http.NotFound(writer, request)
		return
	}
	h.serveMessage(writer, request)
}

func (h *resumableLegacySSEFixture) serveStream(writer http.ResponseWriter, request *http.Request) {
	stream := &legacyResumeStream{events: make(chan string, 16), disconnect: make(chan struct{})}
	lastEventID := request.Header.Get("Last-Event-ID")
	h.mu.Lock()
	h.stream = stream
	h.lastEventID = lastEventID
	pending := h.pending
	if lastEventID != "" {
		h.pending = ""
	}
	endpoint := h.endpoint
	h.mu.Unlock()

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	if lastEventID == "" {
		_, _ = fmt.Fprintf(writer, "event: endpoint\ndata: %s\n\n", endpoint)
	} else {
		h.mu.Lock()
		expectedLastEventID := h.expectedLastEventID
		h.mu.Unlock()
		if lastEventID != expectedLastEventID || pending == "" {
			http.Error(writer, fmt.Sprintf("invalid resume cursor %q (pending=%t)", lastEventID, pending != ""), http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(writer, pending)
		h.resumeOnce.Do(func() { close(h.resumed) })
	}
	writer.(http.Flusher).Flush()
	for {
		select {
		case event := <-stream.events:
			_, _ = io.WriteString(writer, event)
			writer.(http.Flusher).Flush()
		case <-stream.disconnect:
			return
		case <-request.Context().Done():
			return
		}
	}
}

func (h *resumableLegacySSEFixture) serveMessage(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	var rpc struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	if len(rpc.ID) == 0 {
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	result := "{}"
	switch rpc.Method {
	case "server/discover":
		result = `{"error":{"code":-32601,"message":"legacy SSE fixture"}}`
	case "initialize":
		result = `{"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{},"resources":{},"prompts":{}},"serverInfo":{"name":"resumable-legacy","version":"1.0.0"}}}`
	case "tools/list":
		result = `{"result":{"tools":[{"name":"resumed","description":"delivered after SSE recovery","inputSchema":{"type":"object"}}]}}`
	case "resources/list":
		result = `{"result":{"resources":[]}}`
	case "resources/templates/list":
		result = `{"result":{"resourceTemplates":[]}}`
	case "prompts/list":
		result = `{"result":{"prompts":[]}}`
	}
	payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,%s}`, rpc.ID, result[1:len(result)-1])

	h.mu.Lock()
	h.nextEventID++
	eventID := h.nextEventID
	event := fmt.Sprintf("event: message\nid: %d\nretry: 1\ndata: %s\n\n", eventID, payload)
	stream := h.stream
	if rpc.Method == "tools/list" && !h.dropped {
		h.dropped = true
		h.pending = event
		h.expectedLastEventID = fmt.Sprint(eventID - 1)
		stream.once.Do(func() { close(stream.disconnect) })
		h.mu.Unlock()
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	h.mu.Unlock()
	stream.events <- event
	writer.WriteHeader(http.StatusAccepted)
}

type legacyOnlySSEHandler struct {
	base   http.Handler
	ready  chan struct{}
	once   sync.Once
	mu     sync.Mutex
	stream *lockedSSEWriter
}

func newLegacyOnlySSEHandler(base http.Handler) *legacyOnlySSEHandler {
	return &legacyOnlySSEHandler{base: base, ready: make(chan struct{})}
}

func (h *legacyOnlySSEHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet {
		stream := &lockedSSEWriter{ResponseWriter: writer}
		h.mu.Lock()
		h.stream = stream
		h.mu.Unlock()
		h.once.Do(func() { close(h.ready) })
		h.base.ServeHTTP(stream, request)
		return
	}
	body, _ := io.ReadAll(request.Body)
	request.Body = io.NopCloser(bytes.NewReader(body))
	var rpc struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(body, &rpc)
	if rpc.Method != "server/discover" {
		h.base.ServeHTTP(writer, request)
		return
	}
	select {
	case <-h.ready:
	case <-request.Context().Done():
		http.Error(writer, request.Context().Err().Error(), http.StatusRequestTimeout)
		return
	}
	h.mu.Lock()
	stream := h.stream
	h.mu.Unlock()
	payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"legacy SSE fixture"}}`, rpc.ID)
	stream.event(payload)
	writer.WriteHeader(http.StatusAccepted)
}

type lockedSSEWriter struct {
	http.ResponseWriter
	mu sync.Mutex
}

func (w *lockedSSEWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ResponseWriter.WriteHeader(status)
}

func (w *lockedSSEWriter) Write(value []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ResponseWriter.Write(value)
}

func (w *lockedSSEWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ResponseWriter.(http.Flusher).Flush()
}

func (w *lockedSSEWriter) event(payload string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = fmt.Fprintf(w.ResponseWriter, "event: message\ndata: %s\n\n", payload)
	w.ResponseWriter.(http.Flusher).Flush()
}
