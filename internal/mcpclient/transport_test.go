package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"Eylu/internal/config"
)

func TestBuildTransportTypes(t *testing.T) {
	workspace := t.TempDir()
	oauthHandler := &staticOAuthHandler{token: &oauth2.Token{AccessToken: "oauth-token"}}
	tests := []struct {
		name  string
		cfg   config.MCPServerConfig
		check func(*testing.T, sdkmcp.Transport)
	}{
		{
			name: "stdio",
			cfg:  config.MCPServerConfig{Transport: config.MCPTransportStdio, Command: os.Args[0], Args: []string{"-test.run=^$"}},
			check: func(t *testing.T, transport sdkmcp.Transport) {
				command, ok := transport.(*sdkmcp.CommandTransport)
				if !ok {
					t.Fatalf("transport type = %T", transport)
				}
				assertSameFile(t, "command path", command.Command.Path, os.Args[0])
				assertSameFile(t, "command directory", command.Command.Dir, workspace)
			},
		},
		{
			name: "streamable HTTP",
			cfg: config.MCPServerConfig{
				Transport: config.MCPTransportStreamableHTTP,
				URL:       "https://mcp.example.test/rpc",
				OAuth:     &config.MCPOAuthConfig{},
			},
			check: func(t *testing.T, transport sdkmcp.Transport) {
				streamable, ok := transport.(*sdkmcp.StreamableClientTransport)
				if !ok {
					t.Fatalf("transport type = %T", transport)
				}
				if streamable.Endpoint != "https://mcp.example.test/rpc" || streamable.HTTPClient == nil || streamable.HTTPClient.Timeout != 0 {
					t.Fatalf("streamable transport = %#v", streamable)
				}
				if streamable.OAuthHandler != oauthHandler {
					t.Fatal("streamable OAuth handler was not passed to the SDK")
				}
			},
		},
		{
			name: "SSE",
			cfg: config.MCPServerConfig{
				Transport: config.MCPTransportSSE,
				URL:       "https://mcp.example.test/events",
				OAuth:     &config.MCPOAuthConfig{},
			},
			check: func(t *testing.T, transport sdkmcp.Transport) {
				sse, ok := transport.(*resumableSSEClientTransport)
				if !ok {
					t.Fatalf("transport type = %T", transport)
				}
				if sse.Endpoint != "https://mcp.example.test/events" || sse.HTTPClient == nil || sse.HTTPClient.Timeout != 0 {
					t.Fatalf("SSE transport = %#v", sse)
				}
				roundTripper, ok := sse.HTTPClient.Transport.(*headerRoundTripper)
				if !ok || roundTripper.oauthHandler != oauthHandler {
					t.Fatalf("SSE round tripper = %#v", sse.HTTPClient.Transport)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport, err := buildTransport(context.Background(), "fixture", test.cfg, workspace, oauthHandler)
			if err != nil {
				t.Fatal(err)
			}
			test.check(t, transport)
		})
	}
}

func assertSameFile(t *testing.T, label, got, want string) {
	t.Helper()
	gotInfo, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat %s %q: %v", label, got, err)
	}
	wantInfo, err := os.Stat(want)
	if err != nil {
		t.Fatalf("stat expected %s %q: %v", label, want, err)
	}
	if !os.SameFile(gotInfo, wantInfo) {
		t.Fatalf("%s = %q, want same file as %q", label, got, want)
	}
}

func TestHTTPTransportInjectsHeadersWithoutReplacingSDKHeaders(t *testing.T) {
	t.Setenv("MCP_SECRET_HEADER", "environment-secret")
	t.Setenv("MCP_BEARER_TOKEN", "bearer-secret")
	cfg := config.MCPServerConfig{
		Transport:              config.MCPTransportStreamableHTTP,
		URL:                    "https://mcp.example.test/rpc",
		Headers:                map[string]string{"X-Static": "static-value", "MCP-Protocol-Version": "configured-version"},
		EnvironmentHeaders:     map[string]string{"X-Environment": "MCP_SECRET_HEADER"},
		BearerTokenEnvironment: "MCP_BEARER_TOKEN",
	}
	transport, err := buildTransport(context.Background(), "fixture", cfg, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	streamable := transport.(*sdkmcp.StreamableClientTransport)
	roundTripper := streamable.HTTPClient.Transport.(*headerRoundTripper)
	capture := &captureRoundTripper{}
	roundTripper.base = capture
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, cfg.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Mcp-Session-Id", "sdk-session")
	request.Header.Set("MCP-Protocol-Version", "sdk-version")
	request.Header.Set("Last-Event-ID", "sdk-event")
	if _, err := streamable.HTTPClient.Do(request); err != nil {
		t.Fatal(err)
	}
	got := capture.request.Header
	for header, want := range map[string]string{
		"X-Static":             "static-value",
		"X-Environment":        "environment-secret",
		"Authorization":        "Bearer bearer-secret",
		"Mcp-Session-Id":       "sdk-session",
		"MCP-Protocol-Version": "sdk-version",
		"Last-Event-ID":        "sdk-event",
	} {
		if value := got.Get(header); value != want {
			t.Errorf("%s = %q, want %q", header, value, want)
		}
	}
}

func TestSSETransportInjectsOAuthHeader(t *testing.T) {
	oauthHandler := &staticOAuthHandler{token: &oauth2.Token{AccessToken: "oauth-secret", TokenType: "Bearer"}}
	cfg := config.MCPServerConfig{Transport: config.MCPTransportSSE, URL: "https://mcp.example.test/events", OAuth: &config.MCPOAuthConfig{}}
	transport, err := buildTransport(context.Background(), "fixture", cfg, t.TempDir(), oauthHandler)
	if err != nil {
		t.Fatal(err)
	}
	sse := transport.(*resumableSSEClientTransport)
	roundTripper := sse.HTTPClient.Transport.(*headerRoundTripper)
	capture := &captureRoundTripper{}
	roundTripper.base = capture
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, cfg.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sse.HTTPClient.Do(request); err != nil {
		t.Fatal(err)
	}
	if got := capture.request.Header.Get("Authorization"); got != "Bearer oauth-secret" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestLegacySSEDecoderTracksResumeMetadata(t *testing.T) {
	decoder := newLegacySSEDecoder(strings.NewReader(": keepalive\r\nid: event-7\r\nretry: 25\r\nevent: message\r\ndata: first\r\ndata: second\r\n\r\n"))
	event, err := decoder.next()
	if err != nil {
		t.Fatal(err)
	}
	if event.name != "message" || !event.idSet || event.id != "event-7" || !event.retrySet || event.retry.String() != "25ms" || string(event.data) != "first\nsecond" {
		t.Fatalf("event = %#v", event)
	}
	if _, err := decoder.next(); !errors.Is(err, io.EOF) {
		t.Fatalf("second decode error = %v, want EOF", err)
	}
	clamped, err := newLegacySSEDecoder(strings.NewReader("retry: 60000\n\n")).next()
	if err != nil || clamped.retry != legacySSEMaximumReconnectDelay {
		t.Fatalf("clamped retry = %v, error = %v", clamped.retry, err)
	}
}

func TestResumableSSEConnectionCloseStopsReader(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	body := &closeTrackingReadCloser{ReadCloser: reader, closed: make(chan struct{})}
	transport := &resumableSSEClientTransport{
		Endpoint: "https://mcp.example.test/events",
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       body,
				Request:    request,
			}, nil
		})},
	}
	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writer, "event: endpoint\ndata: /messages\n\n")
		writeDone <- err
	}()
	connection, err := transport.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := connection.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	wait.Wait()
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("SSE response body was not closed")
	}
	if _, err := connection.Read(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("Read after Close = %v, want EOF", err)
	}
}

func TestHTTPTransportErrorsAreRedactedAndUnwrap(t *testing.T) {
	const secret = "transport-secret-value"
	cfg := config.MCPServerConfig{
		Transport: config.MCPTransportStreamableHTTP,
		URL:       "https://mcp.example.test/rpc",
		Headers:   map[string]string{"X-API-Key": secret},
	}
	transport, err := buildTransport(context.Background(), "fixture", cfg, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	streamable := transport.(*sdkmcp.StreamableClientTransport)
	roundTripper := streamable.HTTPClient.Transport.(*headerRoundTripper)
	sentinel := errors.New("upstream failure")
	roundTripper.base = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("header %s: %w", request.Header.Get("X-API-Key"), sentinel)
	})
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, cfg.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = streamable.HTTPClient.Do(request)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error = %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error does not unwrap sentinel: %v", err)
	}
}

func TestBuildTransportRejectsMissingSecretsWithoutLeakingValues(t *testing.T) {
	const environment = "EYLU_TEST_MISSING_MCP_SECRET"
	t.Setenv(environment, "secret-that-must-not-leak")
	if err := os.Unsetenv(environment); err != nil {
		t.Fatal(err)
	}
	cfg := config.MCPServerConfig{
		Transport:          config.MCPTransportStreamableHTTP,
		URL:                "https://mcp.example.test/rpc",
		EnvironmentHeaders: map[string]string{"X-API-Key": environment},
	}
	_, err := buildTransport(context.Background(), "fixture", cfg, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), environment) {
		t.Fatalf("error = %v", err)
	}
}

type staticOAuthHandler struct {
	token *oauth2.Token
}

var _ auth.OAuthHandler = (*staticOAuthHandler)(nil)

func (h *staticOAuthHandler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	if h.token == nil {
		return nil, nil
	}
	return oauth2.StaticTokenSource(h.token), nil
}

func (*staticOAuthHandler) Authorize(context.Context, *http.Request, *http.Response) error {
	return nil
}

type captureRoundTripper struct {
	request *http.Request
}

func (c *captureRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	c.request = request
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       http.NoBody,
		Request:    request,
	}, nil
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type closeTrackingReadCloser struct {
	io.ReadCloser
	closed chan struct{}
	once   sync.Once
}

func (c *closeTrackingReadCloser) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.ReadCloser.Close()
}
