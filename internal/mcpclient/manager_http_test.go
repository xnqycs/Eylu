package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"Eylu/internal/config"
)

func TestManagerStreamableHTTPJSONAndSSEProtocol(t *testing.T) {
	for _, jsonResponse := range []bool{true, false} {
		name := "sse"
		if jsonResponse {
			name = "json"
		}
		t.Run(name, func(t *testing.T) {
			server := newPagedProtocolServer()
			handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{JSONResponse: jsonResponse})
			capture := &protocolCapture{handler: handler}
			httpServer := httptest.NewServer(capture)
			defer httpServer.Close()

			manager, diagnostics, err := Open(context.Background(), map[string]config.MCPServerConfig{
				"remote": {Transport: config.MCPTransportStreamableHTTP, URL: httpServer.URL, Headers: map[string]string{"X-Eylu-Test": "present"}, StartupTimeoutSeconds: 5},
			}, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			if len(diagnostics) != 0 {
				t.Fatalf("diagnostics=%#v", diagnostics)
			}
			detail, err := manager.Inspect("remote")
			if err != nil {
				t.Fatal(err)
			}
			if detail.ProtocolVersion != "2025-11-25" || detail.ServerInfo.Tools != 2 || len(detail.Resources) != 2 || len(detail.ResourceTemplates) != 2 || len(detail.Prompts) != 2 {
				t.Fatalf("detail=%#v", detail)
			}
			result, err := manager.ReadResource(context.Background(), "remote", "fixture://one")
			if err != nil || len(result.Contents) != 1 || result.Contents[0].Text != "fixture://one" {
				t.Fatalf("resource=%#v error=%v", result, err)
			}
			observed := capture.snapshot()
			if !observed.staticHeader || !observed.protocol20251125 || len(observed.responseSessions) == 0 || len(observed.requestSessions) == 0 {
				t.Fatalf("transport capture=%#v", observed)
			}
			wantContentType := "text/event-stream"
			if jsonResponse {
				wantContentType = "application/json"
			}
			if !containsPrefix(observed.contentTypes, wantContentType) {
				t.Fatalf("content types=%#v, want %s", observed.contentTypes, wantContentType)
			}
		})
	}
}

func TestManagerRebuildsExpiredStreamableHTTPSession(t *testing.T) {
	server := newPagedProtocolServer()
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{JSONResponse: true})
	capture := &protocolCapture{handler: handler}
	httpServer := httptest.NewServer(capture)
	defer httpServer.Close()
	manager, _, err := Open(context.Background(), map[string]config.MCPServerConfig{
		"remote": {Transport: config.MCPTransportStreamableHTTP, URL: httpServer.URL, StartupTimeoutSeconds: 5},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	capture.expireNextResource.Store(true)
	result, err := manager.ReadResource(context.Background(), "remote", "fixture://one")
	if err != nil || len(result.Contents) != 1 || result.Contents[0].Text != "fixture://one" {
		t.Fatalf("resource=%#v error=%v", result, err)
	}
	observed := capture.snapshot()
	if len(observed.responseSessions) < 2 {
		t.Fatalf("session was not rebuilt: %#v", observed.responseSessions)
	}
}

func newPagedProtocolServer() *sdkmcp.Server {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "http-fixture", Version: "1.0.0"}, &sdkmcp.ServerOptions{PageSize: 1})
	type input struct {
		Value string `json:"value"`
	}
	for _, name := range []string{"one", "two"} {
		toolName := name
		sdkmcp.AddTool(server, &sdkmcp.Tool{Name: toolName, Description: toolName}, func(_ context.Context, _ *sdkmcp.CallToolRequest, value input) (*sdkmcp.CallToolResult, input, error) {
			return nil, value, nil
		})
		uri := "fixture://" + name
		server.AddResource(&sdkmcp.Resource{URI: uri, Name: name}, func(_ context.Context, request *sdkmcp.ReadResourceRequest) (*sdkmcp.ReadResourceResult, error) {
			return &sdkmcp.ReadResourceResult{Contents: []*sdkmcp.ResourceContents{{URI: request.Params.URI, Text: request.Params.URI}}}, nil
		})
		server.AddResourceTemplate(&sdkmcp.ResourceTemplate{URITemplate: "fixture://" + name + "/{id}", Name: name}, nil)
		server.AddPrompt(&sdkmcp.Prompt{Name: name}, func(_ context.Context, request *sdkmcp.GetPromptRequest) (*sdkmcp.GetPromptResult, error) {
			return &sdkmcp.GetPromptResult{Messages: []*sdkmcp.PromptMessage{{Role: sdkmcp.Role("user"), Content: &sdkmcp.TextContent{Text: request.Params.Name}}}}, nil
		})
	}
	return server
}

type protocolCapture struct {
	handler            http.Handler
	expireNextResource atomic.Bool
	mu                 sync.Mutex
	staticHeader       bool
	protocol20251125   bool
	responseSessions   map[string]struct{}
	requestSessions    map[string]struct{}
	contentTypes       []string
}

type protocolCaptureSnapshot struct {
	staticHeader     bool
	protocol20251125 bool
	responseSessions []string
	requestSessions  []string
	contentTypes     []string
}

func (c *protocolCapture) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var body []byte
	if request.Body != nil {
		body, _ = io.ReadAll(request.Body)
		request.Body = io.NopCloser(bytes.NewReader(body))
	}
	var rpc struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(body, &rpc)
	if rpc.Method == "server/discover" {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"legacy protocol fixture"}}`, rpc.ID)
		return
	}
	if rpc.Method == "resources/read" && c.expireNextResource.CompareAndSwap(true, false) {
		http.Error(writer, "expired session", http.StatusNotFound)
		return
	}
	c.mu.Lock()
	if request.Header.Get("X-Eylu-Test") == "present" {
		c.staticHeader = true
	}
	if request.Header.Get("MCP-Protocol-Version") == "2025-11-25" {
		c.protocol20251125 = true
	}
	if session := request.Header.Get("Mcp-Session-Id"); session != "" {
		if c.requestSessions == nil {
			c.requestSessions = make(map[string]struct{})
		}
		c.requestSessions[session] = struct{}{}
	}
	c.mu.Unlock()
	c.handler.ServeHTTP(&captureResponseWriter{ResponseWriter: writer, capture: c}, request)
}

func (c *protocolCapture) recordResponse(header http.Header) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if session := header.Get("Mcp-Session-Id"); session != "" {
		if c.responseSessions == nil {
			c.responseSessions = make(map[string]struct{})
		}
		c.responseSessions[session] = struct{}{}
	}
	if contentType := header.Get("Content-Type"); contentType != "" {
		c.contentTypes = append(c.contentTypes, contentType)
	}
}

func (c *protocolCapture) snapshot() protocolCaptureSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := protocolCaptureSnapshot{staticHeader: c.staticHeader, protocol20251125: c.protocol20251125, contentTypes: append([]string(nil), c.contentTypes...)}
	for session := range c.responseSessions {
		result.responseSessions = append(result.responseSessions, session)
	}
	for session := range c.requestSessions {
		result.requestSessions = append(result.requestSessions, session)
	}
	sort.Strings(result.responseSessions)
	sort.Strings(result.requestSessions)
	return result
}

type captureResponseWriter struct {
	http.ResponseWriter
	capture *protocolCapture
	once    sync.Once
}

func (w *captureResponseWriter) record() { w.once.Do(func() { w.capture.recordResponse(w.Header()) }) }
func (w *captureResponseWriter) WriteHeader(status int) {
	w.record()
	w.ResponseWriter.WriteHeader(status)
}
func (w *captureResponseWriter) Write(value []byte) (int, error) {
	w.record()
	return w.ResponseWriter.Write(value)
}
func (w *captureResponseWriter) Flush() {
	w.record()
	w.ResponseWriter.(http.Flusher).Flush()
}

func containsPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(strings.ToLower(value), prefix) {
			return true
		}
	}
	return false
}
