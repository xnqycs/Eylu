package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"Eylu/internal/config"
	"Eylu/internal/logging"
)

const mcpCommandTerminateTimeout = 2 * time.Second

func buildTransport(ctx context.Context, name string, cfg config.MCPServerConfig, workspace string, oauthHandler auth.OAuthHandler) (sdkmcp.Transport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch cfg.EffectiveTransport() {
	case config.MCPTransportStdio:
		workingDirectory, err := resolveWorkingDirectory(workspace, cfg.WorkingDirectory)
		if err != nil {
			return nil, err
		}
		command := exec.Command(cfg.Command, cfg.Args...)
		command.Dir = workingDirectory
		command.Env = serverEnvironment(cfg.Environment)
		return &sdkmcp.CommandTransport{Command: command, TerminateDuration: mcpCommandTerminateTimeout}, nil
	case config.MCPTransportStreamableHTTP:
		if cfg.OAuth != nil && oauthHandler == nil {
			return nil, fmt.Errorf("MCP server %q OAuth handler is required", name)
		}
		httpClient, err := newMCPHTTPClient(name, cfg, nil)
		if err != nil {
			return nil, err
		}
		return &sdkmcp.StreamableClientTransport{
			Endpoint:     cfg.URL,
			HTTPClient:   httpClient,
			OAuthHandler: oauthHandlerForConfig(cfg, oauthHandler),
		}, nil
	case config.MCPTransportSSE:
		if cfg.OAuth != nil && oauthHandler == nil {
			return nil, fmt.Errorf("MCP server %q OAuth handler is required", name)
		}
		httpClient, err := newMCPHTTPClient(name, cfg, oauthHandlerForConfig(cfg, oauthHandler))
		if err != nil {
			return nil, err
		}
		return &sdkmcp.SSEClientTransport{Endpoint: cfg.URL, HTTPClient: httpClient}, nil
	default:
		return nil, fmt.Errorf("MCP server %q transport %q is unsupported", name, cfg.Transport)
	}
}

func oauthHandlerForConfig(cfg config.MCPServerConfig, handler auth.OAuthHandler) auth.OAuthHandler {
	if cfg.OAuth == nil {
		return nil
	}
	return handler
}

func newMCPHTTPClient(name string, cfg config.MCPServerConfig, oauthHandler auth.OAuthHandler) (*http.Client, error) {
	headers, secrets, err := configuredMCPHeaders(name, cfg)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport: &headerRoundTripper{
			base:         http.DefaultTransport,
			headers:      headers,
			secrets:      secrets,
			oauthHandler: oauthHandler,
		},
	}, nil
}

func configuredMCPHeaders(name string, cfg config.MCPServerConfig) (http.Header, []string, error) {
	headers := make(http.Header, len(cfg.Headers)+len(cfg.EnvironmentHeaders)+1)
	secrets := make([]string, 0, len(cfg.Headers)+len(cfg.EnvironmentHeaders)+1)
	for header, value := range cfg.Headers {
		if strings.ContainsAny(value, "\r\n") {
			return nil, nil, fmt.Errorf("MCP server %q header %q contains a line break", name, header)
		}
		headers.Set(header, value)
		secrets = append(secrets, value)
	}
	for header, environment := range cfg.EnvironmentHeaders {
		value, ok := os.LookupEnv(environment)
		if !ok {
			return nil, nil, fmt.Errorf("MCP server %q environment header %q requires environment variable %s", name, header, environment)
		}
		if strings.ContainsAny(value, "\r\n") {
			return nil, nil, fmt.Errorf("MCP server %q environment header %q contains a line break", name, header)
		}
		headers.Set(header, value)
		secrets = append(secrets, value)
	}
	if environment := cfg.BearerTokenEnvironment; environment != "" {
		token, ok := os.LookupEnv(environment)
		if !ok {
			return nil, nil, fmt.Errorf("MCP server %q bearer token requires environment variable %s", name, environment)
		}
		token = strings.TrimSpace(token)
		if token == "" || strings.ContainsAny(token, "\r\n") {
			return nil, nil, fmt.Errorf("MCP server %q bearer token environment variable %s is empty or invalid", name, environment)
		}
		headers.Set("Authorization", "Bearer "+token)
		secrets = append(secrets, token, "Bearer "+token)
	}
	return headers, secrets, nil
}

type headerRoundTripper struct {
	base         http.RoundTripper
	headers      http.Header
	secrets      []string
	oauthHandler auth.OAuthHandler
}

func (t *headerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	prepared := request.Clone(request.Context())
	prepared.Header = request.Header.Clone()
	for header, values := range t.headers {
		canonical := http.CanonicalHeaderKey(header)
		if _, exists := prepared.Header[canonical]; exists {
			continue
		}
		prepared.Header[canonical] = append([]string(nil), values...)
	}
	secrets := append([]string(nil), t.secrets...)
	if t.oauthHandler != nil {
		tokenSource, err := t.oauthHandler.TokenSource(request.Context())
		if err != nil {
			return nil, redactTransportError(err, secrets)
		}
		if tokenSource != nil {
			token, err := tokenSource.Token()
			if err != nil {
				return nil, redactTransportError(err, secrets)
			}
			if token != nil && token.AccessToken != "" {
				token.SetAuthHeader(prepared)
				secrets = append(secrets, token.AccessToken, prepared.Header.Get("Authorization"))
			}
		}
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	response, err := base.RoundTrip(prepared)
	if err != nil {
		return nil, redactTransportError(err, secrets)
	}
	return response, nil
}

type redactedTransportError struct {
	cause   error
	secrets []string
}

func (e *redactedTransportError) Error() string {
	return logging.Redact(e.cause.Error(), e.secrets...)
}

func (e *redactedTransportError) Unwrap() error {
	return e.cause
}

func redactTransportError(err error, secrets []string) error {
	if err == nil || errors.As(err, new(*redactedTransportError)) {
		return err
	}
	return &redactedTransportError{cause: err, secrets: append([]string(nil), secrets...)}
}
