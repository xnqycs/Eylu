package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

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
				if command.Command.Path != os.Args[0] || command.Command.Dir != workspace {
					t.Fatalf("command = %#v", command.Command)
				}
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
				sse, ok := transport.(*sdkmcp.SSEClientTransport)
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
	sse := transport.(*sdkmcp.SSEClientTransport)
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
