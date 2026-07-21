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
