package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"Eylu/internal/config"
	"Eylu/internal/logging"
)

const (
	mcpCommandTerminateTimeout     = 2 * time.Second
	mcpHTTPSessionDeleteTimeout    = 2 * time.Second
	mcpHTTPUserAgent               = "Eylu/1.0.0"
	legacySSEDefaultReconnectDelay = time.Second
	legacySSEMaximumReconnectDelay = 30 * time.Second
	legacySSEMaximumReconnectTries = 5
	legacySSEIncomingMessageBuffer = 100
)

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
			MaxRetries:   maxConnectionRetries,
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
		return &resumableSSEClientTransport{Endpoint: cfg.URL, HTTPClient: httpClient}, nil
	default:
		return nil, fmt.Errorf("MCP server %q transport %q is unsupported", name, cfg.Transport)
	}
}

// resumableSSEClientTransport implements the legacy 2024-11-05 SSE transport.
// The upstream SDK connection closes on the first interrupted GET, so this
// implementation preserves the logical connection and resumes streams that
// provide event IDs.
type resumableSSEClientTransport struct {
	Endpoint   string
	HTTPClient *http.Client
}

var _ sdkmcp.Transport = (*resumableSSEClientTransport)(nil)

func (t *resumableSSEClientTransport) Connect(ctx context.Context) (sdkmcp.Connection, error) {
	endpoint, err := url.Parse(t.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint: %w", err)
	}
	connectionCtx, cancel := context.WithCancel(ctx)
	response, err := t.openStream(connectionCtx, "")
	if err != nil {
		cancel()
		return nil, err
	}
	decoder := newLegacySSEDecoder(response.Body)
	first, err := decoder.next()
	if err != nil {
		response.Body.Close()
		cancel()
		return nil, fmt.Errorf("missing endpoint: %w", err)
	}
	if first.name != "endpoint" || !first.hasData {
		response.Body.Close()
		cancel()
		return nil, fmt.Errorf("first event is %q, want %q", first.name, "endpoint")
	}
	messageEndpoint, err := endpoint.Parse(string(first.data))
	if err != nil {
		response.Body.Close()
		cancel()
		return nil, fmt.Errorf("invalid message endpoint: %w", err)
	}
	connection := &resumableSSEConnection{
		transport:       t,
		ctx:             connectionCtx,
		cancel:          cancel,
		messageEndpoint: messageEndpoint,
		incoming:        make(chan []byte, legacySSEIncomingMessageBuffer),
		done:            make(chan struct{}),
		body:            response.Body,
		reconnectDelay:  legacySSEDefaultReconnectDelay,
	}
	connection.applyEventMetadata(first)
	go connection.readLoop(response.Body, decoder)
	return connection, nil
}

func (t *resumableSSEClientTransport) openStream(ctx context.Context, lastEventID string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, t.Endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	client := t.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		response.Body.Close()
		return nil, fmt.Errorf("failed to connect: %s", response.Status)
	}
	return response, nil
}

type resumableSSEConnection struct {
	transport *resumableSSEClientTransport
	ctx       context.Context
	cancel    context.CancelFunc
	incoming  chan []byte
	done      chan struct{}

	mu              sync.Mutex
	messageEndpoint *url.URL
	body            io.ReadCloser
	lastEventID     string
	reconnectDelay  time.Duration
	terminalError   error
	closed          bool
	closeOnce       sync.Once
}

var _ sdkmcp.Connection = (*resumableSSEConnection)(nil)

func (*resumableSSEConnection) SessionID() string { return "" }

func (c *resumableSSEConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case data := <-c.incoming:
		return jsonrpc.DecodeMessage(data)
	case <-c.done:
		c.mu.Lock()
		err := c.terminalError
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
}

func (c *resumableSSEConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	data, err := jsonrpc.EncodeMessage(message)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return io.EOF
	}
	messageEndpoint := *c.messageEndpoint
	c.mu.Unlock()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, messageEndpoint.String(), bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	client := c.transport.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("failed to write: %s", response.Status)
	}
	return nil
}

func (c *resumableSSEConnection) Close() error {
	c.finish(nil)
	return nil
}

func (c *resumableSSEConnection) finish(err error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.terminalError = err
		body := c.body
		c.mu.Unlock()
		c.cancel()
		if body != nil {
			_ = body.Close()
		}
		close(c.done)
	})
}

func (c *resumableSSEConnection) readLoop(body io.ReadCloser, decoder *legacySSEDecoder) {
	failedResumes := 0
	streamStartID := c.resumeState()
	for {
		event, err := decoder.next()
		if err == nil {
			c.applyEventMetadata(event)
			if event.hasData {
				switch event.name {
				case "", "message":
					select {
					case c.incoming <- event.data:
					case <-c.done:
						return
					}
				case "endpoint":
					c.updateMessageEndpoint(event.data)
				}
			}
			continue
		}
		_ = body.Close()
		if c.ctx.Err() != nil {
			c.finish(nil)
			return
		}
		lastEventID, delay := c.resumeParameters()
		if lastEventID == "" {
			c.finish(fmt.Errorf("legacy SSE stream ended without a resumable event ID: %w", err))
			return
		}
		if lastEventID == streamStartID {
			failedResumes++
		} else {
			failedResumes = 0
		}
		lastResumeError := err
		for {
			if failedResumes >= legacySSEMaximumReconnectTries {
				c.finish(fmt.Errorf("legacy SSE stream made no progress after %d resume attempts: %w", failedResumes, lastResumeError))
				return
			}
			if err := waitForSSEReconnect(c.ctx, delay); err != nil {
				c.finish(nil)
				return
			}
			response, reconnectErr := c.transport.openStream(c.ctx, lastEventID)
			if reconnectErr != nil {
				failedResumes++
				lastResumeError = reconnectErr
				delay = nextSSEReconnectDelay(delay)
				continue
			}
			if !c.replaceBody(response.Body) {
				response.Body.Close()
				return
			}
			body = response.Body
			decoder = newLegacySSEDecoder(body)
			streamStartID = lastEventID
			break
		}
	}
}

func (c *resumableSSEConnection) applyEventMetadata(event legacySSEEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if event.idSet {
		c.lastEventID = event.id
	}
	if event.retrySet {
		c.reconnectDelay = event.retry
	}
}

func (c *resumableSSEConnection) updateMessageEndpoint(data []byte) {
	endpoint, err := url.Parse(string(data))
	if err != nil {
		return
	}
	c.mu.Lock()
	c.messageEndpoint = c.messageEndpoint.ResolveReference(endpoint)
	c.mu.Unlock()
}

func (c *resumableSSEConnection) resumeState() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastEventID
}

func (c *resumableSSEConnection) resumeParameters() (string, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastEventID, c.reconnectDelay
}

func (c *resumableSSEConnection) replaceBody(body io.ReadCloser) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.body = body
	return true
}

func nextSSEReconnectDelay(delay time.Duration) time.Duration {
	delay *= 2
	if delay > legacySSEMaximumReconnectDelay {
		delay = legacySSEMaximumReconnectDelay
	}
	return delay
}

func waitForSSEReconnect(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type legacySSEEvent struct {
	name     string
	id       string
	idSet    bool
	data     []byte
	hasData  bool
	retry    time.Duration
	retrySet bool
}

type legacySSEDecoder struct {
	reader *bufio.Reader
	eof    bool
}

func newLegacySSEDecoder(reader io.Reader) *legacySSEDecoder {
	return &legacySSEDecoder{reader: bufio.NewReader(reader)}
}

func (d *legacySSEDecoder) next() (legacySSEEvent, error) {
	if d.eof {
		return legacySSEEvent{}, io.EOF
	}
	var event legacySSEEvent
	var data []string
	for {
		line, err := d.reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return legacySSEEvent{}, fmt.Errorf("read SSE event: %w", err)
		}
		if errors.Is(err, io.EOF) {
			d.eof = true
		}
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			if event.hasData || event.idSet || event.retrySet || event.name != "" {
				event.data = []byte(strings.Join(data, "\n"))
				return event, nil
			}
			if d.eof {
				return legacySSEEvent{}, io.EOF
			}
			continue
		}
		if !strings.HasPrefix(line, ":") {
			field, value, found := strings.Cut(line, ":")
			if !found {
				value = ""
			}
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				event.name = value
			case "data":
				event.hasData = true
				data = append(data, value)
			case "id":
				if !strings.ContainsRune(value, '\x00') {
					event.id, event.idSet = value, true
				}
			case "retry":
				milliseconds, parseErr := strconv.ParseInt(value, 10, 64)
				if parseErr == nil && milliseconds >= 0 {
					milliseconds = min(milliseconds, int64(legacySSEMaximumReconnectDelay/time.Millisecond))
					event.retry = time.Duration(milliseconds) * time.Millisecond
					event.retrySet = true
				}
			}
		}
		if d.eof {
			if event.hasData || event.idSet || event.retrySet || event.name != "" {
				event.data = []byte(strings.Join(data, "\n"))
				return event, nil
			}
			return legacySSEEvent{}, io.EOF
		}
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
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("initialize MCP cookie jar: %w", err)
	}
	return &http.Client{
		Jar: jar,
		Transport: &headerRoundTripper{
			base:           http.DefaultTransport,
			headers:        headers,
			secrets:        secrets,
			oauthHandler:   oauthHandler,
			requestTimeout: cfg.StartupTimeout(defaultStartupTimeout),
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
	base           http.RoundTripper
	headers        http.Header
	secrets        []string
	oauthHandler   auth.OAuthHandler
	requestTimeout time.Duration
}

func (t *headerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	requestContext := request.Context()
	cancel := func() {}
	if request.Method == http.MethodDelete {
		if _, hasDeadline := requestContext.Deadline(); !hasDeadline {
			requestContext, cancel = context.WithTimeout(requestContext, mcpHTTPSessionDeleteTimeout)
		}
	} else if request.Method == http.MethodPost && t.requestTimeout > 0 {
		if _, hasDeadline := requestContext.Deadline(); !hasDeadline {
			requestContext, cancel = context.WithTimeout(requestContext, t.requestTimeout)
		}
	}
	defer cancel()
	prepared := request.Clone(requestContext)
	prepared.Header = request.Header.Clone()
	for header, values := range t.headers {
		canonical := http.CanonicalHeaderKey(header)
		if _, exists := prepared.Header[canonical]; exists {
			continue
		}
		prepared.Header[canonical] = append([]string(nil), values...)
	}
	if prepared.Header.Get("User-Agent") == "" {
		prepared.Header.Set("User-Agent", mcpHTTPUserAgent)
	}
	secrets := append([]string(nil), t.secrets...)
	if t.oauthHandler != nil {
		tokenSource, err := t.oauthHandler.TokenSource(prepared.Context())
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
