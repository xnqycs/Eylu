package mcpclient

//lint:file-ignore SA1019 MCP protocol 2025-11-25 requires roots, sampling, and logging support.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdkjsonrpc "github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sync/errgroup"

	"Eylu/internal/config"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

const (
	maxToolsPerServer              = 256
	maxResourcesPerServer          = 512
	maxResourceTemplatesPerServer  = 512
	maxPromptsPerServer            = 256
	maxInstructionsBytes           = 64 << 10
	maxSchemaBytes                 = 256 << 10
	maxResourceCatalog             = 128 << 10
	maxToolDescription             = 16 << 10
	maxLocalToolNameBytes          = 64
	defaultStartupTimeout          = 60 * time.Second
	defaultOptionalCatalogTimeout  = 10 * time.Second
	defaultCallTimeout             = 60 * time.Second
	maxConnectionRetries           = 3
	maxConcurrentServerConnections = 4
	connectionRetryBaseDelay       = 200 * time.Millisecond
)

type ServerStatus string

const (
	StatusDisabled     ServerStatus = "disabled"
	StatusConnecting   ServerStatus = "connecting"
	StatusConnected    ServerStatus = "connected"
	StatusNeedsAuth    ServerStatus = "needs_auth"
	StatusReconnecting ServerStatus = "reconnecting"
	StatusDisconnected ServerStatus = "disconnected"
	StatusError        ServerStatus = "error"
)

type EventKind string

const (
	EventStatus         EventKind = "status"
	EventCatalogChanged EventKind = "catalog_changed"
	EventDiagnostic     EventKind = "diagnostic"
	EventProgress       EventKind = "progress"
	EventLogging        EventKind = "logging"
	EventResourceUpdate EventKind = "resource_updated"
)

type Event struct {
	Time        time.Time    `json:"time"`
	Kind        EventKind    `json:"kind"`
	Server      string       `json:"server"`
	Status      ServerStatus `json:"status,omitempty"`
	Message     string       `json:"message,omitempty"`
	Data        any          `json:"data,omitempty"`
	Fingerprint string       `json:"fingerprint,omitempty"`
}

type Diagnostic struct {
	Server  string    `json:"server"`
	Message string    `json:"message"`
	Time    time.Time `json:"time,omitempty"`
}

type Context struct {
	Server          string                    `json:"server"`
	ProtocolVersion string                    `json:"protocol_version"`
	Instructions    string                    `json:"instructions,omitempty"`
	ToolDefinitions []protocol.ToolDefinition `json:"tool_definitions,omitempty"`
	ResourceCatalog string                    `json:"resource_catalog,omitempty"`
}

type ServerInfo struct {
	Name              string       `json:"name"`
	Status            ServerStatus `json:"status"`
	Transport         string       `json:"transport"`
	Enabled           bool         `json:"enabled"`
	Required          bool         `json:"required,omitempty"`
	Implementation    string       `json:"implementation,omitempty"`
	Version           string       `json:"version,omitempty"`
	ProtocolVersion   string       `json:"protocol_version,omitempty"`
	Tools             int          `json:"tools"`
	Resources         int          `json:"resources"`
	ResourceTemplates int          `json:"resource_templates"`
	Prompts           int          `json:"prompts"`
	LastError         string       `json:"last_error,omitempty"`
	ConnectDurationMS int64        `json:"connect_duration_ms,omitempty"`
}

type ToolInfo struct {
	Name         string                    `json:"name"`
	LocalName    string                    `json:"local_name"`
	Description  string                    `json:"description,omitempty"`
	Annotations  *protocol.ToolAnnotations `json:"annotations,omitempty"`
	InputSchema  json.RawMessage           `json:"input_schema,omitempty"`
	OutputSchema json.RawMessage           `json:"output_schema,omitempty"`
	Permission   string                    `json:"permission"`
	Status       string                    `json:"status"`
}

type ResourceInfo struct {
	URI         string              `json:"uri"`
	Name        string              `json:"name,omitempty"`
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	MIMEType    string              `json:"mime_type,omitempty"`
	Size        int64               `json:"size,omitempty"`
	Annotations *sdkmcp.Annotations `json:"annotations,omitempty"`
}

type ResourceTemplateInfo struct {
	URITemplate string              `json:"uri_template"`
	Name        string              `json:"name,omitempty"`
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	MIMEType    string              `json:"mime_type,omitempty"`
	Annotations *sdkmcp.Annotations `json:"annotations,omitempty"`
}

type PromptInfo struct {
	Name        string                   `json:"name"`
	Title       string                   `json:"title,omitempty"`
	Description string                   `json:"description,omitempty"`
	Arguments   []*sdkmcp.PromptArgument `json:"arguments,omitempty"`
}

type ServerDetail struct {
	ServerInfo
	Instructions      string                     `json:"instructions,omitempty"`
	Capabilities      *sdkmcp.ServerCapabilities `json:"capabilities,omitempty"`
	Tools             []ToolInfo                 `json:"tools,omitempty"`
	Resources         []ResourceInfo             `json:"resources,omitempty"`
	ResourceTemplates []ResourceTemplateInfo     `json:"resource_templates,omitempty"`
	Prompts           []PromptInfo               `json:"prompts,omitempty"`
	Diagnostics       []Diagnostic               `json:"diagnostics,omitempty"`
	Config            map[string]any             `json:"config,omitempty"`
}

// Options wires host callbacks into the SDK. Capabilities are inferred from
// non-nil handlers so Eylu never advertises an unimplemented callback.
type Options struct {
	CreateMessageHandler          func(context.Context, *sdkmcp.CreateMessageRequest) (*sdkmcp.CreateMessageResult, error)
	CreateMessageWithToolsHandler func(context.Context, *sdkmcp.CreateMessageWithToolsRequest) (*sdkmcp.CreateMessageWithToolsResult, error)
	ElicitationHandler            func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error)
	ElicitationForm               bool
	ElicitationURL                bool
	OAuthClient                   *OAuthClient
}

type Manager struct {
	mu          sync.RWMutex
	ctx         context.Context
	cancel      context.CancelFunc
	workspace   string
	options     Options
	servers     map[string]*serverRuntime
	tools       []tool.Tool
	contexts    []Context
	fingerprint string
	subscribers map[uint64]chan Event
	nextSubID   uint64
	closed      bool
}

type serverRuntime struct {
	mu              sync.RWMutex
	connectMu       sync.Mutex
	refreshMu       sync.Mutex
	manager         *Manager
	name            string
	config          config.MCPServerConfig
	status          ServerStatus
	session         *sdkmcp.ClientSession
	client          *sdkmcp.Client
	callCtx         context.Context
	cancelCalls     context.CancelFunc
	cancelConnect   context.CancelFunc
	cancelTransport context.CancelFunc
	generation      uint64
	readOnly        map[string]bool
	tools           []*sdkmcp.Tool
	toolInfo        []ToolInfo
	toolAdapters    []tool.Tool
	resources       []*sdkmcp.Resource
	templates       []*sdkmcp.ResourceTemplate
	prompts         []*sdkmcp.Prompt
	context         Context
	implementation  string
	version         string
	protocolVersion string
	instructions    string
	capabilities    *sdkmcp.ServerCapabilities
	lastError       string
	connectDuration time.Duration
	diagnostics     []Diagnostic
}

type catalogSnapshot struct {
	tools        []*sdkmcp.Tool
	toolInfo     []ToolInfo
	toolAdapters []tool.Tool
	resources    []*sdkmcp.Resource
	templates    []*sdkmcp.ResourceTemplate
	prompts      []*sdkmcp.Prompt
	context      Context
	warnings     []error
}

func Open(ctx context.Context, servers map[string]config.MCPServerConfig, workspace string) (*Manager, []Diagnostic, error) {
	return OpenWithOptions(ctx, servers, workspace, Options{})
}

func OpenWithOptions(ctx context.Context, servers map[string]config.MCPServerConfig, workspace string, options Options) (*Manager, []Diagnostic, error) {
	managerCtx, cancel := context.WithCancel(context.Background())
	manager := &Manager{
		ctx: managerCtx, cancel: cancel, workspace: workspace, options: options,
		servers: make(map[string]*serverRuntime, len(servers)), subscribers: make(map[uint64]chan Event),
	}
	names := sortedServerNames(servers)
	for _, name := range names {
		cfg := servers[name]
		status := StatusDisabled
		if cfg.IsEnabled() {
			status = StatusDisconnected
		}
		manager.servers[name] = &serverRuntime{manager: manager, name: name, config: cfg, status: status, readOnly: stringSet(cfg.ReadOnlyTools)}
	}
	manager.rebuildCatalog()
	connectionErrors := make([]error, len(names))
	semaphore := make(chan struct{}, maxConcurrentServerConnections)
	var connections sync.WaitGroup
	for index, name := range names {
		runtime := manager.servers[name]
		if !runtime.config.IsEnabled() {
			continue
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			connectionErrors[index] = manager.connect(ctx, name, StatusConnecting)
		}()
	}
	connections.Wait()
	diagnostics := make([]Diagnostic, 0)
	for index, name := range names {
		if err := connectionErrors[index]; err != nil {
			runtime := manager.servers[name]
			diagnostic := Diagnostic{Server: name, Message: err.Error(), Time: time.Now().UTC()}
			diagnostics = append(diagnostics, diagnostic)
			if runtime.config.Required {
				_ = manager.Close()
				return nil, diagnostics, fmt.Errorf("required MCP server %s: %w", name, err)
			}
		}
	}
	return manager, diagnostics, nil
}

func sortedServerNames(servers map[string]config.MCPServerConfig) []string {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *Manager) connect(ctx context.Context, name string, targetStatus ServerStatus) error {
	runtime, err := m.runtime(name)
	if err != nil {
		return err
	}
	runtime.connectMu.Lock()
	defer runtime.connectMu.Unlock()
	runtime.mu.RLock()
	generation := runtime.generation
	runtime.mu.RUnlock()
	for retry := 0; ; retry++ {
		status := targetStatus
		if retry > 0 {
			status = StatusReconnecting
		}
		err = m.connectAttempt(ctx, runtime, name, status)
		if err == nil {
			return nil
		}
		if retry >= maxConnectionRetries || !retryableConnectionError(ctx, m.ctx, err) {
			return m.connectionFailed(runtime, generation, err)
		}
		delay := connectionRetryBaseDelay << retry
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return m.connectionFailed(runtime, generation, ctx.Err())
		case <-m.ctx.Done():
			timer.Stop()
			return m.connectionFailed(runtime, generation, m.ctx.Err())
		}
	}
}

func (m *Manager) connectAttempt(ctx context.Context, runtime *serverRuntime, name string, targetStatus ServerStatus) error {
	m.mu.RLock()
	runtime.mu.Lock()
	if m.closed {
		runtime.mu.Unlock()
		m.mu.RUnlock()
		return errors.New("MCP manager is closed")
	}
	if !runtime.config.IsEnabled() {
		runtime.status = StatusDisabled
		runtime.mu.Unlock()
		m.mu.RUnlock()
		m.publish(Event{Kind: EventStatus, Server: name, Status: StatusDisabled})
		return nil
	}
	attemptGeneration := runtime.generation
	cfg := runtime.config
	connectCtx, cancelConnect := managedTimeout(ctx, m.ctx, cfg.StartupTimeout(defaultStartupTimeout))
	runtime.cancelConnect = cancelConnect
	runtime.status = targetStatus
	runtime.lastError = ""
	runtime.mu.Unlock()
	m.mu.RUnlock()
	defer func() {
		cancelConnect()
		runtime.mu.Lock()
		runtime.cancelConnect = nil
		runtime.mu.Unlock()
	}()
	m.publish(Event{Kind: EventStatus, Server: name, Status: targetStatus})

	started := time.Now()

	var oauthHandler sdkauth.OAuthHandler
	if cfg.OAuth != nil {
		oauthClient, oauthErr := m.oauthClient()
		if oauthErr != nil {
			return oauthErr
		}
		handler, handlerErr := oauthClient.SDKHandler(connectCtx, oauthOptions(name, cfg))
		if handlerErr != nil && !errors.Is(handlerErr, ErrCredentialsNotFound) {
			return handlerErr
		}
		oauthHandler = handler
	}
	transport, err := buildTransport(connectCtx, name, cfg, m.workspace, oauthHandler)
	if err != nil {
		return err
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "eylu", Version: "1.0.0"}, m.clientOptions(name))
	if root, rootErr := workspaceRoot(m.workspace); rootErr == nil {
		client.AddRoots(root)
	}
	sessionOptions, err := pinnedClientSessionOptions()
	if err != nil {
		return err
	}
	var cancelTransport context.CancelFunc
	var session *sdkmcp.ClientSession
	if cfg.EffectiveTransport() == config.MCPTransportSSE {
		sessionCtx, cancelSession := context.WithCancel(m.ctx)
		cancelTransport = cancelSession
		type connectResult struct {
			session *sdkmcp.ClientSession
			err     error
		}
		connected := make(chan connectResult, 1)
		go func() {
			value, connectErr := client.Connect(sessionCtx, transport, sessionOptions)
			connected <- connectResult{session: value, err: connectErr}
		}()
		select {
		case result := <-connected:
			session, err = result.session, result.err
		case <-connectCtx.Done():
			cancelSession()
			result := <-connected
			err = errors.Join(connectCtx.Err(), result.err)
		}
	} else if cfg.EffectiveTransport() == config.MCPTransportStreamableHTTP {
		sessionCtx, cancelSession := managedCancellation(ctx, m.ctx)
		session, err = client.Connect(sessionCtx, transport, sessionOptions)
		cancelSession()
	} else {
		session, err = client.Connect(connectCtx, transport, sessionOptions)
	}
	if err != nil {
		if cancelTransport != nil {
			cancelTransport()
		}
		return fmt.Errorf("connect MCP server: %w", err)
	}
	initialized := session.InitializeResult()
	if initialized == nil || initialized.ServerInfo == nil {
		if cancelTransport != nil {
			cancelTransport()
		}
		_ = session.Close()
		return errors.New("MCP server returned incomplete initialization metadata")
	}
	catalogCtx, cancelCatalog := managedTimeout(ctx, m.ctx, cfg.StartupTimeout(defaultStartupTimeout))
	snapshot, err := m.fetchCatalog(catalogCtx, runtime, session, initialized.Capabilities, initialized.ProtocolVersion, initialized.Instructions, true, false)
	cancelCatalog()
	if err != nil {
		if cancelTransport != nil {
			cancelTransport()
		}
		_ = session.Close()
		return err
	}
	m.mu.RLock()
	runtime.mu.Lock()
	if m.closed || runtime.generation != attemptGeneration || !runtime.config.IsEnabled() {
		runtime.mu.Unlock()
		m.mu.RUnlock()
		if cancelTransport != nil {
			cancelTransport()
		}
		_ = session.Close()
		return context.Canceled
	}
	oldSession := runtime.session
	oldCancelTransport := runtime.cancelTransport
	if runtime.cancelCalls != nil {
		runtime.cancelCalls()
	}
	callCtx, cancelCalls := context.WithCancel(m.ctx)
	runtime.callCtx, runtime.cancelCalls = callCtx, cancelCalls
	runtime.cancelTransport = cancelTransport
	runtime.session, runtime.client = session, client
	runtime.generation++
	generation := runtime.generation
	runtime.status = StatusConnected
	runtime.lastError = ""
	runtime.connectDuration = time.Since(started)
	runtime.implementation = initialized.ServerInfo.Name
	runtime.version = initialized.ServerInfo.Version
	runtime.protocolVersion = initialized.ProtocolVersion
	runtime.instructions = limitUTF8(initialized.Instructions, maxInstructionsBytes)
	runtime.capabilities = initialized.Capabilities
	runtime.applySnapshot(snapshot)
	warningDiagnostics := make([]Diagnostic, 0, len(snapshot.warnings))
	for _, warning := range snapshot.warnings {
		diagnostic := Diagnostic{Server: name, Message: warning.Error(), Time: time.Now().UTC()}
		runtime.diagnostics = appendLimitedDiagnostics(runtime.diagnostics, diagnostic)
		warningDiagnostics = append(warningDiagnostics, diagnostic)
	}
	runtime.mu.Unlock()
	m.mu.RUnlock()
	if oldSession != nil && oldSession != session {
		if oldCancelTransport != nil {
			oldCancelTransport()
		}
		_ = oldSession.Close()
	}
	m.rebuildCatalog()
	m.publish(Event{Kind: EventStatus, Server: name, Status: StatusConnected})
	for _, diagnostic := range warningDiagnostics {
		m.publish(Event{Kind: EventDiagnostic, Server: name, Status: StatusConnected, Message: diagnostic.Message})
	}
	go m.monitor(runtime, session, generation)
	if hasBackgroundStartup(initialized.Capabilities) {
		go m.loadOptionalCatalog(runtime, session, generation)
	}
	return nil
}

func retryableConnectionError(callerContext, managerContext context.Context, err error) bool {
	if err == nil || callerContext.Err() != nil || managerContext.Err() != nil || errors.Is(err, context.Canceled) || isAuthorizationError(err) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, transient := range []string{
		"408", "429", "500", "502", "503", "504",
		"bad gateway", "connection refused", "connection reset", "connection aborted",
		"temporary", "timeout", "timed out", "unexpected eof", "server closed",
		"dial tcp", "no such host", "network is unreachable", "broken pipe", "wsarecv", "wsasend",
	} {
		if strings.Contains(message, transient) {
			return true
		}
	}
	return false
}

func managedTimeout(parent, managerContext context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	phaseContext, cancel := context.WithTimeout(parent, timeout)
	stopManagerCancel := context.AfterFunc(managerContext, cancel)
	return phaseContext, func() {
		stopManagerCancel()
		cancel()
	}
}

func managedCancellation(parent, managerContext context.Context) (context.Context, context.CancelFunc) {
	phaseContext, cancel := context.WithCancel(context.WithoutCancel(parent))
	stopParentCancel := context.AfterFunc(parent, cancel)
	stopManagerCancel := context.AfterFunc(managerContext, cancel)
	return phaseContext, func() {
		stopParentCancel()
		stopManagerCancel()
		cancel()
	}
}

func (m *Manager) connectionFailed(runtime *serverRuntime, generation uint64, err error) error {
	status := StatusError
	if isAuthorizationError(err) {
		status = StatusNeedsAuth
	}
	diagnostic := Diagnostic{Server: runtime.name, Message: err.Error(), Time: time.Now().UTC()}
	m.mu.RLock()
	runtime.mu.Lock()
	if m.closed || runtime.generation != generation || !runtime.config.IsEnabled() {
		runtime.mu.Unlock()
		m.mu.RUnlock()
		return err
	}
	runtime.status = status
	runtime.lastError = err.Error()
	runtime.diagnostics = appendLimitedDiagnostics(runtime.diagnostics, diagnostic)
	runtime.mu.Unlock()
	m.mu.RUnlock()
	m.rebuildCatalog()
	m.publish(Event{Kind: EventDiagnostic, Server: runtime.name, Status: status, Message: err.Error()})
	return err
}

func appendLimitedDiagnostics(values []Diagnostic, value Diagnostic) []Diagnostic {
	values = append(values, value)
	if len(values) > 32 {
		values = append([]Diagnostic(nil), values[len(values)-32:]...)
	}
	return values
}

func isAuthorizationError(err error) bool {
	if errors.Is(err, ErrAuthorizationRequired) || errors.Is(err, ErrCredentialsNotFound) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "401") || strings.Contains(message, "403") || strings.Contains(message, "unauthorized") || strings.Contains(message, "forbidden") || strings.Contains(message, "authorization required")
}

func (m *Manager) clientOptions(name string) *sdkmcp.ClientOptions {
	capabilities := &sdkmcp.ClientCapabilities{RootsV2: &sdkmcp.RootCapabilities{ListChanged: true}}
	if m.options.ElicitationHandler != nil && (m.options.ElicitationForm || m.options.ElicitationURL) {
		capabilities.Elicitation = &sdkmcp.ElicitationCapabilities{}
		if m.options.ElicitationForm {
			capabilities.Elicitation.Form = &sdkmcp.FormElicitationCapabilities{}
		}
		if m.options.ElicitationURL {
			capabilities.Elicitation.URL = &sdkmcp.URLElicitationCapabilities{}
		}
	}
	options := &sdkmcp.ClientOptions{
		Capabilities:                  capabilities,
		CreateMessageHandler:          m.options.CreateMessageHandler,
		CreateMessageWithToolsHandler: m.options.CreateMessageWithToolsHandler,
		ElicitationHandler:            m.options.ElicitationHandler,
		KeepAlive:                     30 * time.Second,
		KeepAliveFailureThreshold:     2,
		ToolListChangedHandler:        func(context.Context, *sdkmcp.ToolListChangedRequest) { m.queueRefresh(name, "tools/list_changed") },
		PromptListChangedHandler:      func(context.Context, *sdkmcp.PromptListChangedRequest) { m.queueRefresh(name, "prompts/list_changed") },
		ResourceListChangedHandler: func(context.Context, *sdkmcp.ResourceListChangedRequest) {
			m.queueRefresh(name, "resources/list_changed")
		},
		ResourceUpdatedHandler: func(_ context.Context, request *sdkmcp.ResourceUpdatedNotificationRequest) {
			m.publish(Event{Kind: EventResourceUpdate, Server: name, Data: request.Params})
			m.queueRefresh(name, "resources/updated")
		},
		LoggingMessageHandler: func(_ context.Context, request *sdkmcp.LoggingMessageRequest) {
			m.publish(Event{Kind: EventLogging, Server: name, Data: request.Params})
		},
		ProgressNotificationHandler: func(_ context.Context, request *sdkmcp.ProgressNotificationClientRequest) {
			m.publish(Event{Kind: EventProgress, Server: name, Data: request.Params})
		},
	}
	return options
}

func workspaceRoot(workspace string) (*sdkmcp.Root, error) {
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	path := filepath.ToSlash(absolute)
	if len(path) >= 2 && path[1] == ':' {
		path = "/" + path
	}
	return &sdkmcp.Root{URI: (&url.URL{Scheme: "file", Path: path}).String(), Name: filepath.Base(absolute)}, nil
}

func (m *Manager) fetchCatalog(ctx context.Context, runtime *serverRuntime, session *sdkmcp.ClientSession, capabilities *sdkmcp.ServerCapabilities, protocolVersion, instructions string, listTools, listOptional bool) (catalogSnapshot, error) {
	runtime.mu.RLock()
	cfg := runtime.config
	runtime.mu.RUnlock()
	snapshot := catalogSnapshot{}
	readOnly := stringSet(cfg.ReadOnlyTools)
	var toolSnapshot catalogSnapshot
	resources := make([]*sdkmcp.Resource, 0)
	templates := make([]*sdkmcp.ResourceTemplate, 0)
	prompts := make([]*sdkmcp.Prompt, 0)
	var resourcesErr, templatesErr, promptsErr error
	if listTools && capabilities != nil && capabilities.Tools != nil {
		for remote, listErr := range session.Tools(ctx, nil) {
			if listErr != nil {
				return catalogSnapshot{}, fmt.Errorf("list MCP tools: %w", listErr)
			}
			if !toolAllowed(remote.Name, cfg.AllowTools, cfg.DenyTools) {
				continue
			}
			if len(toolSnapshot.tools) >= maxToolsPerServer {
				return catalogSnapshot{}, fmt.Errorf("MCP tool limit exceeds %d", maxToolsPerServer)
			}
			definition, localName, convertErr := convertToolDefinition(runtime.name, remote)
			if convertErr != nil {
				return catalogSnapshot{}, convertErr
			}
			permission := "write"
			if readOnly[remote.Name] {
				permission = "read"
			}
			toolSnapshot.tools = append(toolSnapshot.tools, remote)
			toolSnapshot.toolInfo = append(toolSnapshot.toolInfo, ToolInfo{
				Name: remote.Name, LocalName: localName, Description: remote.Description,
				Annotations: definition.Annotations, InputSchema: cloneRaw(definition.InputSchema), OutputSchema: cloneRaw(definition.OutputSchema),
				Permission: permission, Status: "available",
			})
			toolSnapshot.toolAdapters = append(toolSnapshot.toolAdapters, &remoteToolAdapter{server: runtime, remote: remote, definition: definition, readOnly: readOnly[remote.Name]})
			toolSnapshot.context.ToolDefinitions = append(toolSnapshot.context.ToolDefinitions, definition)
		}
	}
	var group errgroup.Group
	if cfg.EffectiveTransport() != config.MCPTransportStreamableHTTP {
		group.SetLimit(1)
	}
	if listOptional && capabilities != nil && capabilities.Resources != nil {
		group.Go(func() error {
			requestCtx, cancel := optionalCatalogContext(ctx, cfg)
			defer cancel()
			for resource, listErr := range session.Resources(requestCtx, nil) {
				if listErr != nil {
					resourcesErr = fmt.Errorf("list MCP resources: %w", listErr)
					return nil
				}
				if len(resources) >= maxResourcesPerServer {
					resourcesErr = fmt.Errorf("MCP resource limit exceeds %d", maxResourcesPerServer)
					return nil
				}
				resources = append(resources, resource)
			}
			return nil
		})
		group.Go(func() error {
			requestCtx, cancel := optionalCatalogContext(ctx, cfg)
			defer cancel()
			for template, listErr := range session.ResourceTemplates(requestCtx, nil) {
				if listErr != nil {
					templatesErr = fmt.Errorf("list MCP resource templates: %w", listErr)
					return nil
				}
				if len(templates) >= maxResourceTemplatesPerServer {
					templatesErr = fmt.Errorf("MCP resource template limit exceeds %d", maxResourceTemplatesPerServer)
					return nil
				}
				templates = append(templates, template)
			}
			return nil
		})
	}
	if listOptional && capabilities != nil && capabilities.Prompts != nil {
		group.Go(func() error {
			requestCtx, cancel := optionalCatalogContext(ctx, cfg)
			defer cancel()
			for prompt, listErr := range session.Prompts(requestCtx, nil) {
				if listErr != nil {
					promptsErr = fmt.Errorf("list MCP prompts: %w", listErr)
					return nil
				}
				if len(prompts) >= maxPromptsPerServer {
					promptsErr = fmt.Errorf("MCP prompt limit exceeds %d", maxPromptsPerServer)
					return nil
				}
				prompts = append(prompts, prompt)
			}
			return nil
		})
	}
	_ = group.Wait()
	snapshot.tools = toolSnapshot.tools
	snapshot.toolInfo = toolSnapshot.toolInfo
	snapshot.toolAdapters = toolSnapshot.toolAdapters
	snapshot.context.ToolDefinitions = toolSnapshot.context.ToolDefinitions
	snapshot.resources = resources
	snapshot.templates = templates
	snapshot.prompts = prompts
	for _, warning := range []error{resourcesErr, templatesErr, promptsErr} {
		if warning != nil {
			snapshot.warnings = append(snapshot.warnings, warning)
		}
	}
	if len(snapshot.resources) > 0 {
		resourceAdapter, definition, convertErr := newResourceToolForResources(runtime, snapshot.resources)
		if convertErr != nil {
			snapshot.warnings = append(snapshot.warnings, fmt.Errorf("build MCP resource adapter: %w", convertErr))
		} else {
			snapshot.toolAdapters = append(snapshot.toolAdapters, resourceAdapter)
			snapshot.context.ToolDefinitions = append(snapshot.context.ToolDefinitions, definition)
		}
	}
	catalog, err := json.Marshal(snapshot.resources)
	if err != nil {
		snapshot.warnings = append(snapshot.warnings, fmt.Errorf("encode MCP resource catalog: %w", err))
		catalog = []byte("[]")
	}
	snapshot.context.Server = runtime.name
	snapshot.context.ProtocolVersion = protocolVersion
	snapshot.context.Instructions = limitUTF8(instructions, maxInstructionsBytes)
	snapshot.context.ResourceCatalog = limitUTF8(string(catalog), maxResourceCatalog)
	return snapshot, nil
}

func hasBackgroundStartup(capabilities *sdkmcp.ServerCapabilities) bool {
	return capabilities != nil && (capabilities.Logging != nil || capabilities.Resources != nil || capabilities.Prompts != nil)
}

func (m *Manager) loadOptionalCatalog(runtime *serverRuntime, session *sdkmcp.ClientSession, generation uint64) {
	runtime.refreshMu.Lock()
	defer runtime.refreshMu.Unlock()
	runtime.mu.RLock()
	if runtime.session != session || runtime.generation != generation || runtime.status != StatusConnected {
		runtime.mu.RUnlock()
		return
	}
	capabilities, protocolVersion, instructions := runtime.capabilities, runtime.protocolVersion, runtime.instructions
	timeout := runtime.config.StartupTimeout(defaultStartupTimeout)
	runtime.mu.RUnlock()
	startupWarnings := make([]error, 0, 1)
	if capabilities != nil && capabilities.Logging != nil {
		loggingCtx, cancelLogging := context.WithTimeout(m.ctx, defaultOptionalCatalogTimeout)
		if err := session.SetLoggingLevel(loggingCtx, &sdkmcp.SetLoggingLevelParams{Level: sdkmcp.LoggingLevel("debug")}); err != nil {
			startupWarnings = append(startupWarnings, fmt.Errorf("set MCP logging level: %w", err))
		}
		cancelLogging()
	}
	ctx, cancel := context.WithTimeout(m.ctx, timeout)
	snapshot, err := m.fetchCatalog(ctx, runtime, session, capabilities, protocolVersion, instructions, false, true)
	cancel()
	if err != nil {
		m.publish(Event{Kind: EventDiagnostic, Server: runtime.name, Status: StatusConnected, Message: err.Error()})
		return
	}
	snapshot.warnings = append(startupWarnings, snapshot.warnings...)
	runtime.mu.Lock()
	if runtime.session != session || runtime.generation != generation || runtime.status != StatusConnected {
		runtime.mu.Unlock()
		return
	}
	runtime.applyOptionalSnapshot(snapshot)
	warningDiagnostics := make([]Diagnostic, 0, len(snapshot.warnings))
	for _, warning := range snapshot.warnings {
		diagnostic := Diagnostic{Server: runtime.name, Message: warning.Error(), Time: time.Now().UTC()}
		runtime.diagnostics = appendLimitedDiagnostics(runtime.diagnostics, diagnostic)
		warningDiagnostics = append(warningDiagnostics, diagnostic)
	}
	runtime.mu.Unlock()
	m.rebuildCatalog()
	m.publish(Event{Kind: EventCatalogChanged, Server: runtime.name, Fingerprint: m.Fingerprint()})
	for _, diagnostic := range warningDiagnostics {
		m.publish(Event{Kind: EventDiagnostic, Server: runtime.name, Status: StatusConnected, Message: diagnostic.Message})
	}
}

func optionalCatalogContext(parent context.Context, cfg config.MCPServerConfig) (context.Context, context.CancelFunc) {
	if cfg.EffectiveTransport() != config.MCPTransportStreamableHTTP {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, defaultOptionalCatalogTimeout)
}

func toolAllowed(name string, allow, deny []string) bool {
	if stringSet(deny)[name] {
		return false
	}
	return len(allow) == 0 || stringSet(allow)[name]
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func cloneRaw(value json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), value...) }

func (runtime *serverRuntime) applySnapshot(snapshot catalogSnapshot) {
	runtime.tools = snapshot.tools
	runtime.toolInfo = snapshot.toolInfo
	runtime.toolAdapters = snapshot.toolAdapters
	runtime.resources = snapshot.resources
	runtime.templates = snapshot.templates
	runtime.prompts = snapshot.prompts
	runtime.context = snapshot.context
}

func (runtime *serverRuntime) applyOptionalSnapshot(snapshot catalogSnapshot) {
	runtime.resources = snapshot.resources
	runtime.templates = snapshot.templates
	runtime.prompts = snapshot.prompts
	toolCount := min(len(runtime.tools), len(runtime.toolAdapters))
	runtime.toolAdapters = append(append([]tool.Tool(nil), runtime.toolAdapters[:toolCount]...), snapshot.toolAdapters...)
	definitionCount := min(len(runtime.tools), len(runtime.context.ToolDefinitions))
	runtime.context.ToolDefinitions = append(append([]protocol.ToolDefinition(nil), runtime.context.ToolDefinitions[:definitionCount]...), snapshot.context.ToolDefinitions...)
	runtime.context.ResourceCatalog = snapshot.context.ResourceCatalog
}

func (m *Manager) queueRefresh(name, reason string) {
	go func() {
		if err := m.refresh(name); err != nil && !errors.Is(err, context.Canceled) {
			m.publish(Event{Kind: EventDiagnostic, Server: name, Message: reason + ": " + err.Error()})
		}
	}()
}

func (m *Manager) refresh(name string) error {
	runtime, err := m.runtime(name)
	if err != nil {
		return err
	}
	runtime.refreshMu.Lock()
	defer runtime.refreshMu.Unlock()
	runtime.mu.RLock()
	session := runtime.session
	capabilities, protocolVersion, instructions := runtime.capabilities, runtime.protocolVersion, runtime.instructions
	timeout := runtime.config.CallTimeout(defaultCallTimeout)
	status := runtime.status
	runtime.mu.RUnlock()
	if session == nil || status != StatusConnected {
		return errors.New("MCP server is not connected")
	}
	ctx, cancel := context.WithTimeout(m.ctx, timeout)
	defer cancel()
	snapshot, err := m.fetchCatalog(ctx, runtime, session, capabilities, protocolVersion, instructions, true, true)
	if err != nil {
		return err
	}
	runtime.mu.Lock()
	if runtime.session != session || runtime.status != StatusConnected {
		runtime.mu.Unlock()
		return context.Canceled
	}
	runtime.applySnapshot(snapshot)
	warningDiagnostics := make([]Diagnostic, 0, len(snapshot.warnings))
	for _, warning := range snapshot.warnings {
		diagnostic := Diagnostic{Server: name, Message: warning.Error(), Time: time.Now().UTC()}
		runtime.diagnostics = appendLimitedDiagnostics(runtime.diagnostics, diagnostic)
		warningDiagnostics = append(warningDiagnostics, diagnostic)
	}
	runtime.mu.Unlock()
	m.rebuildCatalog()
	m.publish(Event{Kind: EventCatalogChanged, Server: name, Fingerprint: m.Fingerprint()})
	for _, diagnostic := range warningDiagnostics {
		m.publish(Event{Kind: EventDiagnostic, Server: name, Status: StatusConnected, Message: diagnostic.Message})
	}
	return nil
}

func (m *Manager) monitor(runtime *serverRuntime, session *sdkmcp.ClientSession, generation uint64) {
	err := session.Wait()
	m.mu.RLock()
	closed := m.closed
	m.mu.RUnlock()
	runtime.mu.Lock()
	if closed || runtime.generation != generation || runtime.session != session || !runtime.config.IsEnabled() {
		runtime.mu.Unlock()
		return
	}
	runtime.session = nil
	if runtime.cancelTransport != nil {
		runtime.cancelTransport()
		runtime.cancelTransport = nil
	}
	if runtime.cancelCalls != nil {
		runtime.cancelCalls()
	}
	status := StatusDisconnected
	if err != nil {
		status = StatusError
		if isAuthorizationError(err) {
			status = StatusNeedsAuth
		}
	}
	runtime.status = status
	if err != nil {
		runtime.lastError = err.Error()
	}
	runtime.mu.Unlock()
	m.rebuildCatalog()
	m.publish(Event{Kind: EventStatus, Server: runtime.name, Status: status, Message: errorString(err)})
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (m *Manager) rebuildCatalog() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	names := make([]string, 0, len(m.servers))
	for name := range m.servers {
		names = append(names, name)
	}
	sort.Strings(names)
	usedNames := make(map[string]string)
	var tools []tool.Tool
	var contexts []Context
	for _, name := range names {
		runtime := m.servers[name]
		runtime.mu.RLock()
		if runtime.status == StatusConnected {
			for _, candidate := range runtime.toolAdapters {
				localName := candidate.Definition().Name
				if _, exists := usedNames[localName]; exists {
					continue
				}
				usedNames[localName] = name
				tools = append(tools, candidate)
			}
			contexts = append(contexts, cloneContext(runtime.context))
		}
		runtime.mu.RUnlock()
	}
	encoded, _ := json.Marshal(contexts)
	digest := sha256.Sum256(encoded)
	m.tools, m.contexts = tools, contexts
	m.fingerprint = hex.EncodeToString(digest[:])
}

func cloneContext(value Context) Context {
	value.ToolDefinitions = append([]protocol.ToolDefinition(nil), value.ToolDefinitions...)
	for index := range value.ToolDefinitions {
		value.ToolDefinitions[index].InputSchema = cloneRaw(value.ToolDefinitions[index].InputSchema)
		value.ToolDefinitions[index].OutputSchema = cloneRaw(value.ToolDefinitions[index].OutputSchema)
	}
	return value
}

func (m *Manager) runtime(name string) (*serverRuntime, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, errors.New("MCP manager is closed")
	}
	runtime := m.servers[name]
	if runtime == nil {
		return nil, fmt.Errorf("configured MCP server %q was not found", name)
	}
	return runtime, nil
}

func (m *Manager) List() []ServerInfo {
	m.mu.RLock()
	runtimes := make([]*serverRuntime, 0, len(m.servers))
	for _, runtime := range m.servers {
		runtimes = append(runtimes, runtime)
	}
	m.mu.RUnlock()
	result := make([]ServerInfo, 0, len(runtimes))
	for _, runtime := range runtimes {
		result = append(result, runtime.info())
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (runtime *serverRuntime) info() ServerInfo {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return ServerInfo{
		Name: runtime.name, Status: runtime.status, Transport: runtime.config.EffectiveTransport(), Enabled: runtime.config.IsEnabled(), Required: runtime.config.Required,
		Implementation: runtime.implementation, Version: runtime.version, ProtocolVersion: runtime.protocolVersion,
		Tools: len(runtime.tools), Resources: len(runtime.resources), ResourceTemplates: len(runtime.templates), Prompts: len(runtime.prompts),
		LastError: runtime.lastError, ConnectDurationMS: runtime.connectDuration.Milliseconds(),
	}
}

func (m *Manager) Inspect(name string) (ServerDetail, error) {
	runtime, err := m.runtime(name)
	if err != nil {
		return ServerDetail{}, err
	}
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	detail := ServerDetail{
		ServerInfo: runtime.infoUnlocked(), Instructions: runtime.instructions, Capabilities: runtime.capabilities,
		Tools: append([]ToolInfo(nil), runtime.toolInfo...), Diagnostics: append([]Diagnostic(nil), runtime.diagnostics...), Config: redactedServerConfig(runtime.config),
	}
	for _, resource := range runtime.resources {
		detail.Resources = append(detail.Resources, resourceInfo(resource))
	}
	for _, template := range runtime.templates {
		detail.ResourceTemplates = append(detail.ResourceTemplates, resourceTemplateInfo(template))
	}
	for _, prompt := range runtime.prompts {
		detail.Prompts = append(detail.Prompts, promptInfo(prompt))
	}
	return detail, nil
}

func (runtime *serverRuntime) infoUnlocked() ServerInfo {
	return ServerInfo{
		Name: runtime.name, Status: runtime.status, Transport: runtime.config.EffectiveTransport(), Enabled: runtime.config.IsEnabled(), Required: runtime.config.Required,
		Implementation: runtime.implementation, Version: runtime.version, ProtocolVersion: runtime.protocolVersion,
		Tools: len(runtime.tools), Resources: len(runtime.resources), ResourceTemplates: len(runtime.templates), Prompts: len(runtime.prompts),
		LastError: runtime.lastError, ConnectDurationMS: runtime.connectDuration.Milliseconds(),
	}
}

func redactedServerConfig(cfg config.MCPServerConfig) map[string]any {
	result := map[string]any{"transport": cfg.EffectiveTransport(), "enabled": cfg.IsEnabled(), "required": cfg.Required}
	if cfg.Command != "" {
		result["command"] = cfg.Command
	}
	if len(cfg.Args) > 0 {
		result["args"] = redactedServerArgs(cfg.Args)
	}
	if cfg.WorkingDirectory != "" {
		result["working_directory"] = cfg.WorkingDirectory
	}
	if cfg.URL != "" {
		result["url"] = redactedServerURL(cfg.URL)
	}
	if len(cfg.Headers) > 0 {
		headers := make(map[string]string, len(cfg.Headers))
		for name := range cfg.Headers {
			headers[name] = redactedConfigValue
		}
		result["headers"] = headers
	}
	if len(cfg.EnvironmentHeaders) > 0 {
		result["environment_headers"] = cfg.EnvironmentHeaders
	}
	if cfg.BearerTokenEnvironment != "" {
		result["bearer_token_environment"] = cfg.BearerTokenEnvironment
	}
	if cfg.OAuth != nil {
		result["oauth"] = map[string]any{
			"issuer": redactedServerURL(cfg.OAuth.Issuer), "client_id": cfg.OAuth.ClientID,
			"scopes": cfg.OAuth.Scopes, "redirect_url": redactedServerURL(cfg.OAuth.RedirectURL),
		}
	}
	return result
}

const redactedConfigValue = "[REDACTED]"

func redactedServerArgs(args []string) []string {
	redacted := make([]string, len(args))
	for index := range redacted {
		redacted[index] = redactedConfigValue
	}
	return redacted
}

func redactedServerURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return redactedConfigValue
	}
	parsed.User = nil
	if parsed.Opaque != "" {
		parsed.Opaque = redactedConfigValue
	}
	segments := strings.Split(parsed.Path, "/")
	for index, segment := range segments {
		if segment != "" {
			segments[index] = redactedConfigValue
		}
	}
	parsed.Path = strings.Join(segments, "/")
	parsed.RawPath = ""
	query, _ := url.ParseQuery(parsed.RawQuery)
	for key, values := range query {
		if len(values) == 0 {
			query[key] = []string{redactedConfigValue}
			continue
		}
		for index := range values {
			values[index] = redactedConfigValue
		}
	}
	parsed.RawQuery = query.Encode()
	if parsed.Fragment != "" || parsed.RawFragment != "" {
		parsed.Fragment = redactedConfigValue
		parsed.RawFragment = ""
	}
	return parsed.String()
}

func (m *Manager) ServerTools(name string) ([]ToolInfo, error) {
	runtime, err := m.runtime(name)
	if err != nil {
		return nil, err
	}
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return append([]ToolInfo(nil), runtime.toolInfo...), nil
}

func (m *Manager) Tool(server, name string) (ToolInfo, error) {
	tools, err := m.ServerTools(server)
	if err != nil {
		return ToolInfo{}, err
	}
	for _, candidate := range tools {
		if candidate.Name == name || candidate.LocalName == name {
			return candidate, nil
		}
	}
	return ToolInfo{}, fmt.Errorf("MCP tool %q was not found on server %q", name, server)
}

func (m *Manager) Resources(name string) ([]ResourceInfo, error) {
	runtime, err := m.runtime(name)
	if err != nil {
		return nil, err
	}
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	result := make([]ResourceInfo, 0, len(runtime.resources))
	for _, resource := range runtime.resources {
		result = append(result, resourceInfo(resource))
	}
	return result, nil
}

func resourceInfo(resource *sdkmcp.Resource) ResourceInfo {
	return ResourceInfo{URI: resource.URI, Name: resource.Name, Title: resource.Title, Description: resource.Description, MIMEType: resource.MIMEType, Size: resource.Size, Annotations: resource.Annotations}
}

func resourceTemplateInfo(template *sdkmcp.ResourceTemplate) ResourceTemplateInfo {
	return ResourceTemplateInfo{URITemplate: template.URITemplate, Name: template.Name, Title: template.Title, Description: template.Description, MIMEType: template.MIMEType, Annotations: template.Annotations}
}

func promptInfo(prompt *sdkmcp.Prompt) PromptInfo {
	return PromptInfo{Name: prompt.Name, Title: prompt.Title, Description: prompt.Description, Arguments: append([]*sdkmcp.PromptArgument(nil), prompt.Arguments...)}
}

func (m *Manager) Prompts(name string) ([]PromptInfo, error) {
	runtime, err := m.runtime(name)
	if err != nil {
		return nil, err
	}
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	result := make([]PromptInfo, 0, len(runtime.prompts))
	for _, prompt := range runtime.prompts {
		result = append(result, promptInfo(prompt))
	}
	return result, nil
}

func (m *Manager) ReadResource(ctx context.Context, server, uri string) (*sdkmcp.ReadResourceResult, error) {
	runtime, err := m.runtime(server)
	if err != nil {
		return nil, err
	}
	result, err := runtime.withSessionCall(ctx, func(callCtx context.Context, session *sdkmcp.ClientSession) (any, error) {
		return session.ReadResource(callCtx, &sdkmcp.ReadResourceParams{URI: uri})
	})
	if err != nil {
		return nil, err
	}
	return result.(*sdkmcp.ReadResourceResult), nil
}

func (m *Manager) GetPrompt(ctx context.Context, server, name string, arguments map[string]string) (*sdkmcp.GetPromptResult, error) {
	runtime, err := m.runtime(server)
	if err != nil {
		return nil, err
	}
	result, err := runtime.withSessionCall(ctx, func(callCtx context.Context, session *sdkmcp.ClientSession) (any, error) {
		return session.GetPrompt(callCtx, &sdkmcp.GetPromptParams{Name: name, Arguments: arguments})
	})
	if err != nil {
		return nil, err
	}
	return result.(*sdkmcp.GetPromptResult), nil
}

func (m *Manager) Complete(ctx context.Context, server string, params *sdkmcp.CompleteParams) (*sdkmcp.CompleteResult, error) {
	runtime, err := m.runtime(server)
	if err != nil {
		return nil, err
	}
	result, err := runtime.withSessionCall(ctx, func(callCtx context.Context, session *sdkmcp.ClientSession) (any, error) {
		return session.Complete(callCtx, params)
	})
	if err != nil {
		return nil, err
	}
	return result.(*sdkmcp.CompleteResult), nil
}

func (m *Manager) Ping(ctx context.Context, server string) error {
	runtime, err := m.runtime(server)
	if err != nil {
		return err
	}
	_, err = runtime.withSessionCall(ctx, func(callCtx context.Context, session *sdkmcp.ClientSession) (any, error) {
		return nil, session.Ping(callCtx, nil)
	})
	return err
}

func (m *Manager) SubscribeResource(ctx context.Context, server, uri string) error {
	runtime, err := m.runtime(server)
	if err != nil {
		return err
	}
	_, err = runtime.withSessionCall(ctx, func(callCtx context.Context, session *sdkmcp.ClientSession) (any, error) {
		return nil, session.Subscribe(callCtx, &sdkmcp.SubscribeParams{URI: uri})
	})
	return err
}

func (m *Manager) UnsubscribeResource(ctx context.Context, server, uri string) error {
	runtime, err := m.runtime(server)
	if err != nil {
		return err
	}
	_, err = runtime.withSessionCall(ctx, func(callCtx context.Context, session *sdkmcp.ClientSession) (any, error) {
		return nil, session.Unsubscribe(callCtx, &sdkmcp.UnsubscribeParams{URI: uri})
	})
	return err
}

func (runtime *serverRuntime) beginCall(ctx context.Context) (context.Context, *sdkmcp.ClientSession, context.CancelFunc, error) {
	runtime.mu.RLock()
	session, lifecycle, status := runtime.session, runtime.callCtx, runtime.status
	timeout := runtime.config.CallTimeout(defaultCallTimeout)
	runtime.mu.RUnlock()
	if session == nil || status != StatusConnected {
		return nil, nil, nil, errors.New("MCP server is not connected")
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	stop := context.AfterFunc(lifecycle, cancel)
	return callCtx, session, func() { stop(); cancel() }, nil
}

func (runtime *serverRuntime) withSessionCall(ctx context.Context, call func(context.Context, *sdkmcp.ClientSession) (any, error)) (any, error) {
	callCtx, session, cancel, err := runtime.beginCall(ctx)
	if err != nil {
		return nil, err
	}
	result, callErr := call(callCtx, session)
	cancel()
	if callErr != nil && runtime.manager != nil {
		if errors.Is(callErr, sdkmcp.ErrSessionMissing) {
			runtime.manager.failSession(runtime, session, callErr)
		} else {
			runtime.manager.publish(Event{Kind: EventDiagnostic, Server: runtime.name, Message: callErr.Error(), Data: protocolErrorData(callErr)})
		}
	}
	return result, callErr
}

func (m *Manager) failSession(runtime *serverRuntime, session *sdkmcp.ClientSession, failure error) {
	runtime.mu.Lock()
	if runtime.session != session {
		runtime.mu.Unlock()
		return
	}
	runtime.session = nil
	cancelTransport := runtime.cancelTransport
	runtime.cancelTransport = nil
	if runtime.cancelCalls != nil {
		runtime.cancelCalls()
		runtime.cancelCalls = nil
	}
	runtime.generation++
	runtime.status = StatusError
	runtime.lastError = failure.Error()
	runtime.tools, runtime.toolInfo, runtime.toolAdapters = nil, nil, nil
	runtime.resources, runtime.templates, runtime.prompts = nil, nil, nil
	runtime.context = Context{Server: runtime.name}
	diagnostic := Diagnostic{Server: runtime.name, Message: failure.Error(), Time: time.Now().UTC()}
	runtime.diagnostics = appendLimitedDiagnostics(runtime.diagnostics, diagnostic)
	runtime.mu.Unlock()
	if cancelTransport != nil {
		cancelTransport()
	}
	m.rebuildCatalog()
	m.publish(Event{Kind: EventDiagnostic, Server: runtime.name, Status: StatusError, Message: failure.Error(), Data: protocolErrorData(failure)})
	m.publish(Event{Kind: EventStatus, Server: runtime.name, Status: StatusError, Message: failure.Error()})
	_ = session.Close()
}

func protocolErrorData(err error) map[string]any {
	result := map[string]any{"error": err.Error()}
	var rpcErr *sdkjsonrpc.Error
	if errors.As(err, &rpcErr) {
		result["code"] = rpcErr.Code
		result["message"] = rpcErr.Message
		if len(rpcErr.Data) > 0 {
			result["data"] = json.RawMessage(append([]byte(nil), rpcErr.Data...))
		}
		result["type"] = "jsonrpc"
		return result
	}
	result["type"] = fmt.Sprintf("%T", err)
	return result
}

func (runtime *serverRuntime) resourcesSnapshot() []*sdkmcp.Resource {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return append([]*sdkmcp.Resource(nil), runtime.resources...)
}

func (m *Manager) Reconnect(ctx context.Context, name string) error {
	if err := m.disconnect(name, StatusDisconnected); err != nil {
		return err
	}
	return m.connect(ctx, name, StatusConnecting)
}

func (m *Manager) Enable(ctx context.Context, name string) error {
	runtime, err := m.runtime(name)
	if err != nil {
		return err
	}
	runtime.mu.Lock()
	runtime.config.Enabled, runtime.config.Disabled = true, false
	runtime.mu.Unlock()
	return m.connect(ctx, name, StatusConnecting)
}

func (m *Manager) Disable(_ context.Context, name string) error {
	runtime, err := m.runtime(name)
	if err != nil {
		return err
	}
	runtime.mu.Lock()
	runtime.config.Enabled, runtime.config.Disabled = false, true
	runtime.mu.Unlock()
	return m.disconnect(name, StatusDisabled)
}

func (m *Manager) disconnect(name string, status ServerStatus) error {
	runtime, err := m.runtime(name)
	if err != nil {
		return err
	}
	runtime.mu.Lock()
	session := runtime.session
	runtime.session = nil
	if runtime.cancelConnect != nil {
		runtime.cancelConnect()
		runtime.cancelConnect = nil
	}
	if runtime.cancelTransport != nil {
		runtime.cancelTransport()
		runtime.cancelTransport = nil
	}
	runtime.generation++
	if runtime.cancelCalls != nil {
		runtime.cancelCalls()
	}
	runtime.status = status
	runtime.tools, runtime.toolInfo, runtime.toolAdapters = nil, nil, nil
	runtime.resources, runtime.templates, runtime.prompts = nil, nil, nil
	runtime.context = Context{Server: name}
	runtime.mu.Unlock()
	m.rebuildCatalog()
	m.publish(Event{Kind: EventStatus, Server: name, Status: status})
	if session != nil {
		if closeErr := session.Close(); closeErr != nil && !isBestEffortSessionCloseError(closeErr) {
			diagnostic := Diagnostic{Server: name, Message: "close MCP session: " + closeErr.Error(), Time: time.Now().UTC()}
			runtime.mu.Lock()
			runtime.diagnostics = appendLimitedDiagnostics(runtime.diagnostics, diagnostic)
			runtime.mu.Unlock()
			m.publish(Event{Kind: EventDiagnostic, Server: name, Status: status, Message: diagnostic.Message})
		}
	}
	return nil
}

func oauthOptions(name string, cfg config.MCPServerConfig) OAuthOptions {
	options := OAuthOptions{ServerName: name, ResourceURL: cfg.URL}
	if cfg.OAuth == nil {
		return options
	}
	options.Issuer = cfg.OAuth.Issuer
	options.ClientID = cfg.OAuth.ClientID
	options.Scopes = append([]string(nil), cfg.OAuth.Scopes...)
	options.RedirectURL = cfg.OAuth.RedirectURL
	if cfg.OAuth.ClientSecretEnvironment != "" {
		options.ClientSecret = os.Getenv(cfg.OAuth.ClientSecretEnvironment)
	}
	return options
}

func (m *Manager) oauthClient() (*OAuthClient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.options.OAuthClient != nil {
		return m.options.OAuthClient, nil
	}
	store, err := DefaultCredentialStore()
	if err != nil {
		return nil, err
	}
	m.options.OAuthClient = NewOAuthClient(store)
	return m.options.OAuthClient, nil
}

func (m *Manager) Login(ctx context.Context, name string) error {
	runtime, err := m.runtime(name)
	if err != nil {
		return err
	}
	runtime.mu.RLock()
	cfg := runtime.config
	runtime.mu.RUnlock()
	if cfg.OAuth == nil {
		return errors.New("MCP server does not configure OAuth")
	}
	client, err := m.oauthClient()
	if err != nil {
		return err
	}
	if _, err := client.Authorize(ctx, oauthOptions(name, cfg)); err != nil {
		return err
	}
	return m.Reconnect(ctx, name)
}

func (m *Manager) Logout(ctx context.Context, name string) error {
	runtime, err := m.runtime(name)
	if err != nil {
		return err
	}
	runtime.mu.RLock()
	cfg := runtime.config
	runtime.mu.RUnlock()
	if cfg.OAuth == nil {
		return errors.New("MCP server does not configure OAuth")
	}
	client, clientErr := m.oauthClient()
	var logoutErr error
	if clientErr != nil {
		logoutErr = clientErr
	} else {
		logoutErr = client.Logout(ctx, oauthOptions(name, cfg))
	}
	disconnectErr := m.disconnect(name, StatusNeedsAuth)
	return errors.Join(logoutErr, disconnectErr)
}

func (m *Manager) SubscribeEvents(buffer int) (<-chan Event, func()) {
	if buffer < 1 {
		buffer = 1
	}
	channel := make(chan Event, buffer)
	m.mu.Lock()
	id := m.nextSubID
	m.nextSubID++
	if !m.closed {
		m.subscribers[id] = channel
	} else {
		close(channel)
	}
	m.mu.Unlock()
	var once sync.Once
	return channel, func() { once.Do(func() { m.unsubscribe(id) }) }
}

func (m *Manager) unsubscribe(id uint64) {
	m.mu.Lock()
	channel := m.subscribers[id]
	delete(m.subscribers, id)
	if channel != nil {
		close(channel)
	}
	m.mu.Unlock()
}

func (m *Manager) publish(event Event) {
	event.Time = time.Now().UTC()
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return
	}
	for _, channel := range m.subscribers {
		select {
		case channel <- event:
		default:
		}
	}
}

// Tools keeps the v1 registry API used by the executor integration.
func (m *Manager) Tools() []tool.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]tool.Tool(nil), m.tools...)
}

func (m *Manager) Contexts() []Context {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Context, len(m.contexts))
	for index := range m.contexts {
		result[index] = cloneContext(m.contexts[index])
	}
	return result
}

func (m *Manager) Servers() []ServerInfo {
	result := make([]ServerInfo, 0)
	for _, info := range m.List() {
		if info.Status == StatusConnected {
			result = append(result, info)
		}
	}
	return result
}

func (m *Manager) Fingerprint() string { m.mu.RLock(); defer m.mu.RUnlock(); return m.fingerprint }

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.cancel()
	runtimes := make([]*serverRuntime, 0, len(m.servers))
	for _, runtime := range m.servers {
		runtimes = append(runtimes, runtime)
	}
	for _, channel := range m.subscribers {
		close(channel)
	}
	m.subscribers = nil
	m.tools, m.contexts = nil, nil
	m.mu.Unlock()
	var closeErrors []error
	for _, runtime := range runtimes {
		runtime.mu.Lock()
		session := runtime.session
		runtime.session = nil
		if runtime.cancelConnect != nil {
			runtime.cancelConnect()
			runtime.cancelConnect = nil
		}
		if runtime.cancelTransport != nil {
			runtime.cancelTransport()
			runtime.cancelTransport = nil
		}
		runtime.generation++
		if runtime.cancelCalls != nil {
			runtime.cancelCalls()
		}
		runtime.status = StatusDisconnected
		runtime.mu.Unlock()
		if session != nil {
			if err := session.Close(); err != nil && !isBestEffortSessionCloseError(err) {
				closeErrors = append(closeErrors, fmt.Errorf("close MCP server %s: %w", runtime.name, err))
			}
		}
	}
	return errors.Join(closeErrors...)
}

func isBestEffortSessionCloseError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func resolveWorkingDirectory(workspace, configured string) (string, error) {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve MCP workspace: %w", err)
	}
	candidate := root
	if configured != "" {
		if filepath.IsAbs(configured) {
			candidate = filepath.Clean(configured)
		} else {
			candidate = filepath.Join(root, configured)
		}
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, candidate)
	if err != nil || pathEscapes(relative) {
		return "", errors.New("MCP working directory escapes the workspace")
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return "", fmt.Errorf("MCP working directory is unavailable: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("MCP working directory is not a directory")
	}
	realCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve MCP working directory: %w", err)
	}
	realRelative, err := filepath.Rel(realRoot, realCandidate)
	if err != nil || pathEscapes(realRelative) {
		return "", errors.New("MCP working directory resolves outside the workspace")
	}
	return realCandidate, nil
}

func pathEscapes(relative string) bool {
	return filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func serverEnvironment(names []string) []string {
	allowed := append([]string{"PATH", "SystemRoot", "WINDIR", "HOME", "USERPROFILE", "TMP", "TEMP"}, names...)
	seen := make(map[string]bool, len(allowed))
	environment := make([]string, 0, len(allowed))
	for _, name := range allowed {
		if seen[name] {
			continue
		}
		seen[name] = true
		if value, ok := os.LookupEnv(name); ok {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

func convertToolDefinition(server string, remote *sdkmcp.Tool) (protocol.ToolDefinition, string, error) {
	if remote == nil || strings.TrimSpace(remote.Name) == "" {
		return protocol.ToolDefinition{}, "", errors.New("MCP server returned a tool without a name")
	}
	localName := localToolName(server, remote.Name)
	schema, err := json.Marshal(remote.InputSchema)
	if err != nil || !json.Valid(schema) || len(schema) > maxSchemaBytes {
		return protocol.ToolDefinition{}, "", fmt.Errorf("MCP tool %s has an invalid or oversized input schema", remote.Name)
	}
	if string(schema) == "null" {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	description := limitUTF8(fmt.Sprintf("MCP server %s tool %s. %s", server, remote.Name, remote.Description), maxToolDescription)
	definition := protocol.ToolDefinition{Name: localName, Description: strings.TrimSpace(description), InputSchema: schema}
	definition, err = applyRemoteToolDetails(definition, remote)
	if err != nil {
		return protocol.ToolDefinition{}, "", err
	}
	return definition, localName, nil
}

func sanitizeName(value string) string {
	var result strings.Builder
	for index := 0; index < len(value); index++ {
		current := value[index]
		if current >= 'a' && current <= 'z' || current >= 'A' && current <= 'Z' || current >= '0' && current <= '9' || current == '_' || current == '-' {
			result.WriteByte(current)
		} else {
			result.WriteByte('_')
		}
	}
	if result.Len() == 0 {
		return "unnamed"
	}
	return result.String()
}

func localToolName(server, remote string) string {
	safeServer, safeRemote := sanitizeName(server), sanitizeName(remote)
	name := "mcp__" + safeServer + "__" + safeRemote
	needsSuffix := safeServer != server || safeRemote != remote || len(name) > maxLocalToolNameBytes
	if !needsSuffix {
		return name
	}
	digest := sha256.Sum256([]byte(server + "\x00" + remote))
	suffix := "_" + hex.EncodeToString(digest[:6])
	prefixBytes := maxLocalToolNameBytes - len(suffix)
	if len(name) > prefixBytes {
		name = name[:prefixBytes]
	}
	return name + suffix
}

func limitUTF8(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "\n[truncated]"
}
