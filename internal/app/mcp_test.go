package app

//lint:file-ignore SA1019 MCP protocol 2025-11-25 compatibility.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	contextledger "Eylu/internal/context"
	"Eylu/internal/driver"
	"Eylu/internal/mcpclient"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/tool"
	"Eylu/internal/ui"
)

type appMCPHostDriver struct{}

func (appMCPHostDriver) Name() string { return "app-mcp-host" }
func (appMCPHostDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{}
}
func (appMCPHostDriver) Generate(context.Context, driver.Request, driver.EmitFunc) (protocol.ModelResponse, error) {
	return protocol.ModelResponse{Turn: protocol.Turn{Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "sampled by current provider"}}}, Stop: protocol.StopCompleted}, nil
}

type blockingAppMCPDriver struct {
	started chan struct{}
	release chan struct{}
}

func (*blockingAppMCPDriver) Name() string                      { return "blocking-app-mcp" }
func (*blockingAppMCPDriver) Capabilities() driver.Capabilities { return driver.Capabilities{} }
func (d *blockingAppMCPDriver) Generate(context.Context, driver.Request, driver.EmitFunc) (protocol.ModelResponse, error) {
	close(d.started)
	<-d.release
	return protocol.ModelResponse{Turn: protocol.Turn{Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "done"}}}, Stop: protocol.StopCompleted}, nil
}

type appMCPHostProbe struct {
	Roots  string `json:"roots"`
	Sample string `json:"sample"`
	Form   string `json:"form"`
	URL    string `json:"url"`
}

func TestAppMCPHostAndLiveCatalogUseRealManagerPath(t *testing.T) {
	var capabilitiesMu sync.Mutex
	var capabilities *sdkmcp.ClientCapabilities
	initializedSessions := 0
	var subscribedURI string
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "app-host-fixture", Version: "1.0.0"}, &sdkmcp.ServerOptions{
		InitializedHandler: func(_ context.Context, request *sdkmcp.InitializedRequest) {
			capabilitiesMu.Lock()
			initializedSessions++
			capabilities = request.ClientCapabilities()
			capabilitiesMu.Unlock()
		},
		CompletionHandler: func(_ context.Context, request *sdkmcp.CompleteRequest) (*sdkmcp.CompleteResult, error) {
			return &sdkmcp.CompleteResult{Completion: sdkmcp.CompletionResultDetails{Values: []string{request.Params.Argument.Value + "ylu"}, Total: 1}}, nil
		},
		SubscribeHandler: func(_ context.Context, request *sdkmcp.SubscribeRequest) error {
			subscribedURI = request.Params.URI
			return nil
		},
		UnsubscribeHandler: func(_ context.Context, request *sdkmcp.UnsubscribeRequest) error {
			subscribedURI = "unsubscribed:" + request.Params.URI
			return nil
		},
	})
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "host_probe"}, func(ctx context.Context, request *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, appMCPHostProbe, error) {
		roots, err := request.Session.ListRoots(ctx, nil)
		if err != nil {
			return nil, appMCPHostProbe{}, err
		}
		sampled, err := request.Session.CreateMessage(ctx, &sdkmcp.CreateMessageParams{MaxTokens: 32, Messages: []*sdkmcp.SamplingMessage{{Role: sdkmcp.Role("user"), Content: &sdkmcp.TextContent{Text: "sample"}}}})
		if err != nil {
			return nil, appMCPHostProbe{}, err
		}
		form, err := request.Session.Elicit(ctx, &sdkmcp.ElicitParams{Mode: "form", Message: "Provide name", RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}}})
		if err != nil {
			return nil, appMCPHostProbe{}, err
		}
		urlResult, err := request.Session.Elicit(ctx, &sdkmcp.ElicitParams{Mode: "url", Message: "Open account", URL: "https://example.test/account", ElicitationID: "url-1"})
		if err != nil {
			return nil, appMCPHostProbe{}, err
		}
		return nil, appMCPHostProbe{Roots: roots.Roots[0].URI, Sample: sampled.Content.(*sdkmcp.TextContent).Text, Form: form.Content["name"].(string), URL: urlResult.Action}, nil
	})
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{JSONResponse: true})
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	workspace := t.TempDir()
	appRuntime := &runtime{workspace: workspace, stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, trustPrompted: make(map[string]bool)}
	conversation := agent.NewConversation()
	detach := appRuntime.attachMCPConversation(conversation)
	defer detach()
	defer appRuntime.closeMCP()
	cfg := config.Default()
	cfg.MCPServers = map[string]config.MCPServerConfig{"host": {Transport: config.MCPTransportStreamableHTTP, URL: httpServer.URL, StartupTimeoutSeconds: 5}}
	modelRuntime := agent.Runtime{Provider: provider.Snapshot{Name: "current", Config: config.ProviderConfig{Model: "current-model", BaseURL: "https://provider.test"}}, Driver: appMCPHostDriver{}, PermissionMode: "full"}
	confirmations := 0
	confirm := func(context.Context, policy.Request, policy.Outcome) (tool.Confirmation, error) {
		confirmations++
		return tool.Confirmation{Approved: true}, nil
	}
	ask := func(_ context.Context, request protocol.AskRequest) (protocol.AskResponse, error) {
		if len(request.Questions) != 1 || request.Questions[0].ID != "name" {
			t.Fatalf("questions=%#v", request.Questions)
		}
		return protocol.AskResponse{Answers: map[string][]string{"name": {"Eylu"}}}, nil
	}
	opened := ""
	host := buildMCPHostCallbacks(modelRuntime, confirm, ask, func(target string) error { opened = target; return nil })
	if err := appRuntime.configureMCPRuntimeWithHost(context.Background(), cfg, &modelRuntime, host); err != nil {
		t.Fatal(err)
	}
	initialManager := appRuntime.mcp
	registry := tool.NewRegistry(appRuntime.currentMCPState().Tools...)
	probe, ok := registry.Get("mcp__host__host_probe")
	if !ok {
		t.Fatalf("real app manager tools=%#v", registry.Definitions())
	}
	result := probe.Execute(context.Background(), json.RawMessage(`{}`))
	if result.IsError || !strings.Contains(result.Content, "sampled by current provider") || !strings.Contains(result.Content, "Eylu") || !strings.Contains(result.Content, "file://") || !strings.Contains(result.Content, `"url":"accept"`) {
		t.Fatalf("probe result=%#v", result)
	}
	if opened != "https://example.test/account" || confirmations != 3 {
		t.Fatalf("opened=%q confirmations=%d", opened, confirmations)
	}
	capabilitiesMu.Lock()
	initializedCapabilities := capabilities
	capabilitiesMu.Unlock()
	if initializedCapabilities == nil || initializedCapabilities.RootsV2 == nil || initializedCapabilities.Sampling == nil || initializedCapabilities.Elicitation == nil || initializedCapabilities.Elicitation.Form == nil || initializedCapabilities.Elicitation.URL == nil {
		t.Fatalf("client capabilities=%#v", initializedCapabilities)
	}
	providerManager, err := provider.NewManager(filepath.Join(workspace, "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	tui := &tuiBackend{runtime: appRuntime, manager: providerManager}
	servers, err := tui.MCPServers(context.Background())
	if err != nil || len(servers) != 1 || servers[0].Name != "host" {
		t.Fatalf("TUI MCP poll servers=%#v error=%v", servers, err)
	}
	capabilitiesMu.Lock()
	initializedCount := initializedSessions
	capabilitiesMu.Unlock()
	if appRuntime.mcp != initialManager || initializedCount != 1 {
		t.Fatalf("TUI poll replaced MCP manager/session: manager_same=%t initialized=%d", appRuntime.mcp == initialManager, initializedCount)
	}
	polledRegistry := tool.NewRegistry(appRuntime.currentMCPState().Tools...)
	polledProbe, ok := polledRegistry.Get("mcp__host__host_probe")
	if !ok {
		t.Fatalf("TUI poll removed active MCP tools: %#v", polledRegistry.Definitions())
	}
	polledResult := polledProbe.Execute(context.Background(), json.RawMessage(`{}`))
	if polledResult.IsError || !strings.Contains(polledResult.Content, "sampled by current provider") {
		t.Fatalf("sampling after TUI poll=%#v", polledResult)
	}
	completed, err := tui.handleTUIMCPCommand(context.Background(), []string{"complete", "host", "prompt", "fixture", "name", "E"})
	if err != nil || !strings.Contains(completed, "Eylu") {
		t.Fatalf("TUI completion=%q error=%v", completed, err)
	}
	if _, err := tui.handleTUIMCPCommand(context.Background(), []string{"subscribe", "host", "fixture://host"}); err != nil || subscribedURI != "fixture://host" {
		t.Fatalf("TUI subscribe uri=%q error=%v", subscribedURI, err)
	}
	if _, err := tui.handleTUIMCPCommand(context.Background(), []string{"unsubscribe", "host", "fixture://host"}); err != nil || subscribedURI != "unsubscribed:fixture://host" {
		t.Fatalf("TUI unsubscribe uri=%q error=%v", subscribedURI, err)
	}

	server.AddTool(&sdkmcp.Tool{Name: "added", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "added"}}}, nil
	})
	waitForMCPState(t, appRuntime, func(state agent.MCPRuntimeState) bool { return state.ToolServers["mcp__host__added"] == "host" })
	updated := appRuntime.currentMCPState()
	if updated.Fingerprint == modelRuntime.MCPFingerprint || !conversationHasMCPCategory(conversation.ContextReport()) {
		t.Fatalf("updated fingerprint=%q initial=%q context=%#v", updated.Fingerprint, modelRuntime.MCPFingerprint, conversation.ContextReport())
	}
	if err := appRuntime.mcp.Disable(context.Background(), "host"); err != nil {
		t.Fatal(err)
	}
	waitForMCPState(t, appRuntime, func(state agent.MCPRuntimeState) bool {
		return len(state.Tools) == 0 && state.Fingerprint != updated.Fingerprint
	})
	if conversationHasMCPCategory(conversation.ContextReport()) {
		t.Fatalf("MCP context remained after disable: %#v", conversation.ContextReport())
	}
	blocking := &blockingAppMCPDriver{started: make(chan struct{}), release: make(chan struct{})}
	busyConversation := agent.NewConversation()
	detachBusy := appRuntime.attachMCPConversation(busyConversation)
	defer detachBusy()
	runDone := make(chan error, 1)
	go func() {
		_, runErr := busyConversation.Run(context.Background(), "block", agent.Runtime{Provider: provider.Snapshot{Name: "busy", Config: config.ProviderConfig{Model: "busy"}}, Driver: blocking, PermissionMode: "full"}, &tool.Executor{Registry: tool.NewRegistry(), Policy: policy.AllowAllChecker{}}, agent.LoopOptions{MaxTurns: 1}, false, nil)
		runDone <- runErr
	}()
	<-blocking.started
	stored := make(chan struct{})
	go func() { appRuntime.storeMCPState(agent.MCPRuntimeState{}); close(stored) }()
	select {
	case <-stored:
	case <-time.After(time.Second):
		t.Fatal("storeMCPState blocked on an active conversation")
	}
	closed := make(chan error, 1)
	go func() { closed <- appRuntime.closeMCP() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("closeMCP blocked while a conversation was active")
	}
	close(blocking.release)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

func waitForMCPState(t *testing.T, appRuntime *runtime, condition func(agent.MCPRuntimeState) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition(appRuntime.currentMCPState()) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for MCP state: %#v", appRuntime.currentMCPState())
}

func conversationHasMCPCategory(report contextledger.Report) bool {
	for _, category := range report.Categories {
		if category.Blocks > 0 && (category.Category == contextledger.CategoryMCPInstructions || category.Category == contextledger.CategoryMCPToolSchema || category.Category == contextledger.CategoryMCPResource) {
			return true
		}
	}
	return false
}

type fakeMCPCommandBackend struct {
	calls  []string
	events []mcpclient.Event
}

func (f *fakeMCPCommandBackend) Servers() ([]mcpclient.ServerInfo, string) {
	f.calls = append(f.calls, "list")
	return []mcpclient.ServerInfo{{Name: "alpha", Status: "connected", Transport: "stdio", ProtocolVersion: "2025-11-25", Tools: 1, Resources: 1, Prompts: 1}}, "fingerprint"
}

func (f *fakeMCPCommandBackend) Inspect(name string) (mcpclient.ServerDetail, error) {
	f.calls = append(f.calls, "inspect:"+name)
	return mcpclient.ServerDetail{ServerInfo: mcpclient.ServerInfo{Name: name, Status: "connected"}, Instructions: "stored-secret"}, nil
}

func (f *fakeMCPCommandBackend) Reconnect(_ context.Context, name string) error {
	f.calls = append(f.calls, "reconnect:"+name)
	return nil
}

func (f *fakeMCPCommandBackend) Tools(name string) ([]mcpclient.ToolInfo, error) {
	f.calls = append(f.calls, "tools:"+name)
	return []mcpclient.ToolInfo{{Name: "search", LocalName: "mcp__alpha__search", Description: "stored-secret"}}, nil
}

func (f *fakeMCPCommandBackend) Tool(server, name string) (mcpclient.ToolInfo, error) {
	f.calls = append(f.calls, "tool:"+server+":"+name)
	return mcpclient.ToolInfo{Name: name, LocalName: "mcp__" + server + "__" + name, InputSchema: json.RawMessage(`{"type":"object"}`)}, nil
}

func (f *fakeMCPCommandBackend) Login(_ context.Context, name string) error {
	f.calls = append(f.calls, "login:"+name)
	return nil
}

func (f *fakeMCPCommandBackend) Logout(_ context.Context, name string) error {
	f.calls = append(f.calls, "logout:"+name)
	return nil
}

func (f *fakeMCPCommandBackend) Resources(name string) ([]mcpclient.ResourceInfo, error) {
	f.calls = append(f.calls, "resources:"+name)
	return []mcpclient.ResourceInfo{{URI: "fixture://readme", Name: "readme", MIMEType: "text/plain"}}, nil
}

func (f *fakeMCPCommandBackend) Resource(_ context.Context, server, uri string) (any, error) {
	f.calls = append(f.calls, "resource:"+server+":"+uri)
	return map[string]any{"uri": uri, "text": "stored-secret"}, nil
}

func (f *fakeMCPCommandBackend) Prompts(name string) ([]mcpclient.PromptInfo, error) {
	f.calls = append(f.calls, "prompts:"+name)
	return []mcpclient.PromptInfo{{Name: "review", Description: "review code", Arguments: []*sdkmcp.PromptArgument{{Name: "language"}}}}, nil
}

func (f *fakeMCPCommandBackend) Prompt(_ context.Context, server, name string, arguments map[string]string) (any, error) {
	encoded, _ := json.Marshal(arguments)
	f.calls = append(f.calls, fmt.Sprintf("prompt:%s:%s:%s", server, name, encoded))
	return map[string]any{"description": "stored-secret", "messages": []any{}}, nil
}

func (f *fakeMCPCommandBackend) Complete(_ context.Context, server string, params *sdkmcp.CompleteParams) (any, error) {
	f.calls = append(f.calls, fmt.Sprintf("complete:%s:%s:%s:%s", server, params.Ref.Type, params.Argument.Name, params.Argument.Value))
	return &sdkmcp.CompleteResult{Completion: sdkmcp.CompletionResultDetails{Values: []string{"completed"}, Total: 1}}, nil
}

func (f *fakeMCPCommandBackend) SubscribeResource(_ context.Context, server, uri string) error {
	f.calls = append(f.calls, "subscribe:"+server+":"+uri)
	return nil
}

func (f *fakeMCPCommandBackend) UnsubscribeResource(_ context.Context, server, uri string) error {
	f.calls = append(f.calls, "unsubscribe:"+server+":"+uri)
	return nil
}

func (f *fakeMCPCommandBackend) SubscribeEvents(int) (<-chan mcpclient.Event, func()) {
	events := make(chan mcpclient.Event, len(f.events))
	for _, event := range f.events {
		events <- event
	}
	close(events)
	return events, func() {}
}

func TestMCPCommandRegistersManagementSurface(t *testing.T) {
	runtime := &runtime{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, output: "text"}
	command := runtime.mcpCommand(context.Background())
	want := []string{"complete", "diagnostics", "disable", "enable", "events", "inspect", "list", "login", "logout", "prompt", "prompts", "reconnect", "resource", "resources", "subscribe", "tool", "tools", "unsubscribe"}
	got := make([]string, 0, len(command.Commands()))
	for _, child := range command.Commands() {
		got = append(got, child.Name())
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("commands=%v want=%v", got, want)
	}
}

func TestMCPCommandsRouteArgumentsAndRedactJSON(t *testing.T) {
	tests := []struct {
		args     []string
		wantCall string
	}{
		{args: []string{"list"}, wantCall: "list"},
		{args: []string{"inspect", "alpha"}, wantCall: "inspect:alpha"},
		{args: []string{"reconnect", "alpha"}, wantCall: "reconnect:alpha"},
		{args: []string{"tools", "alpha"}, wantCall: "tools:alpha"},
		{args: []string{"tool", "alpha", "search"}, wantCall: "tool:alpha:search"},
		{args: []string{"login", "alpha"}, wantCall: "login:alpha"},
		{args: []string{"logout", "alpha"}, wantCall: "logout:alpha"},
		{args: []string{"resources", "alpha"}, wantCall: "resources:alpha"},
		{args: []string{"resource", "alpha", "fixture://readme"}, wantCall: "resource:alpha:fixture://readme"},
		{args: []string{"subscribe", "alpha", "fixture://readme"}, wantCall: "subscribe:alpha:fixture://readme"},
		{args: []string{"unsubscribe", "alpha", "fixture://readme"}, wantCall: "unsubscribe:alpha:fixture://readme"},
		{args: []string{"prompts", "alpha"}, wantCall: "prompts:alpha"},
		{args: []string{"prompt", "alpha", "review", "--arguments", `{"language":"go"}`}, wantCall: `prompt:alpha:review:{"language":"go"}`},
		{args: []string{"prompt", "alpha", "review", "language=go"}, wantCall: `prompt:alpha:review:{"language":"go"}`},
		{args: []string{"complete", "alpha", "prompt", "review", "language", "g"}, wantCall: "complete:alpha:ref/prompt:language:g"},
	}
	for _, testCase := range tests {
		t.Run(strings.Join(testCase.args, "_"), func(t *testing.T) {
			backend := &fakeMCPCommandBackend{}
			var stdout bytes.Buffer
			runtime := &runtime{stdout: &stdout, stderr: &bytes.Buffer{}, output: "json", apiKeys: []string{"stored-secret"}}
			command := runtime.mcpCommandWithBackend(context.Background(), func(context.Context) (mcpCommandBackend, error) { return backend, nil }, nil)
			command.SetArgs(testCase.args)
			command.SetOut(&stdout)
			command.SetErr(runtime.stderr)
			if err := command.Execute(); err != nil {
				t.Fatalf("Execute() error=%v", err)
			}
			if len(backend.calls) != 1 || backend.calls[0] != testCase.wantCall {
				t.Fatalf("calls=%v want=%q", backend.calls, testCase.wantCall)
			}
			if strings.Contains(stdout.String(), "stored-secret") || !json.Valid(bytes.TrimSpace(stdout.Bytes())) {
				t.Fatalf("output must be valid redacted JSON: %q", stdout.String())
			}
		})
	}
}

func TestMCPInspectRedactsConfigInJSONAndText(t *testing.T) {
	const secret = "inspect-token-must-stay-secret"
	cfg := config.Default()
	cfg.MCPServers = map[string]config.MCPServerConfig{
		"fixture": {
			Disabled: true,
			Command:  "fixture-command",
			Args:     []string{"--api-key", secret, "--token=" + secret},
			URL:      "https://user:" + secret + "@example.test/mcp?api_key=" + secret,
		},
	}
	manager, _, err := mcpclient.Open(context.Background(), cfg.MCPServers, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	for _, output := range []string{"json", "text"} {
		t.Run(output, func(t *testing.T) {
			var stdout bytes.Buffer
			appRuntime := &runtime{stdout: &stdout, stderr: &bytes.Buffer{}, output: output}
			command := appRuntime.mcpCommandWithBackend(context.Background(), func(context.Context) (mcpCommandBackend, error) {
				return &managerMCPCommandBackend{manager: manager}, nil
			}, nil)
			command.SetArgs([]string{"inspect", "fixture"})
			if err := command.Execute(); err != nil {
				t.Fatal(err)
			}
			result := stdout.String()
			if strings.Contains(result, secret) {
				t.Fatalf("%s inspect exposed secret: %q", output, result)
			}
			for _, expected := range []string{"--api-key", "[REDACTED]"} {
				if !strings.Contains(result, expected) {
					t.Fatalf("%s inspect omitted %q: %q", output, expected, result)
				}
			}
		})
	}
}

func TestOAuthEchoedTokenStaysRedactedAcrossManagerCLIAndTUI(t *testing.T) {
	const token = "oauth-token-must-stay-secret"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(map[string]any{
				"resource": server.URL + "/mcp", "authorization_servers": []string{server.URL + "/issuer"},
			})
		case "/.well-known/oauth-authorization-server/issuer":
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(map[string]any{
				"issuer": server.URL + "/issuer", "authorization_endpoint": server.URL + "/authorize",
				"token_endpoint": server.URL + "/token", "revocation_endpoint": server.URL + "/revoke",
			})
		case "/revoke":
			if err := request.ParseForm(); err != nil {
				t.Errorf("parse revoke form: %v", err)
			}
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprintf(response, `{"error":"temporarily_unavailable","error_description":"rejected %s"}`, request.Form.Get("token"))
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)

	workspace := t.TempDir()
	cfg := config.Default()
	cfg.MCPServers = map[string]config.MCPServerConfig{
		"oauth": {
			Disabled: true, Transport: config.MCPTransportStreamableHTTP, URL: server.URL + "/mcp",
			OAuth: &config.MCPOAuthConfig{Issuer: server.URL + "/issuer", ClientID: "fixture-client"},
		},
	}
	store := mcpclient.NewCredentialStore(filepath.Join(t.TempDir(), "credentials.json"))
	oauthClient := mcpclient.NewOAuthClient(store, mcpclient.WithOAuthHTTPClient(server.Client()))
	manager, _, err := mcpclient.OpenWithOptions(context.Background(), cfg.MCPServers, workspace, mcpclient.Options{OAuthClient: oauthClient})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	oauthOptions := mcpclient.OAuthOptions{ServerName: "oauth", ResourceURL: server.URL + "/mcp", Issuer: server.URL + "/issuer", ClientID: "fixture-client"}
	authHash, err := oauthOptions.AuthHash()
	if err != nil {
		t.Fatal(err)
	}
	credentialKey, err := mcpclient.CredentialKey(oauthOptions.ServerName, oauthOptions.ResourceURL, authHash)
	if err != nil {
		t.Fatal(err)
	}
	seedCredential := func() {
		t.Helper()
		if err := store.Put(context.Background(), credentialKey, mcpclient.OAuthCredential{AccessToken: token, TokenType: "Bearer"}); err != nil {
			t.Fatal(err)
		}
	}
	assertSafeError := func(layer string, err error, diagnostics ...string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s error = nil", layer)
		}
		combined := err.Error() + "\n" + strings.Join(diagnostics, "\n")
		if strings.Contains(combined, token) {
			t.Fatalf("%s exposed OAuth token: %q", layer, combined)
		}
		if !strings.Contains(combined, "502") || !strings.Contains(combined, "temporarily_unavailable") {
			t.Fatalf("%s omitted safe diagnostics: %q", layer, combined)
		}
	}

	seedCredential()
	assertSafeError("manager", manager.Logout(context.Background(), "oauth"))

	seedCredential()
	var cliOut, cliErr bytes.Buffer
	cliRuntime := &runtime{stdout: &cliOut, stderr: &cliErr, output: "json"}
	command := cliRuntime.mcpCommandWithBackend(context.Background(), func(context.Context) (mcpCommandBackend, error) {
		return &managerMCPCommandBackend{manager: manager}, nil
	}, nil)
	command.SetArgs([]string{"logout", "oauth"})
	command.SetOut(&cliOut)
	command.SetErr(&cliErr)
	assertSafeError("CLI", command.Execute(), cliOut.String(), cliErr.String())

	seedCredential()
	providerManager, err := provider.NewManager(filepath.Join(workspace, "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	appRuntime := &runtime{workspace: workspace, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	loadedManager, err := appRuntime.loadMCP(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := loadedManager.Close(); err != nil {
		t.Fatal(err)
	}
	appRuntime.mcpMu.Lock()
	appRuntime.mcp = manager
	appRuntime.mcpMu.Unlock()
	t.Cleanup(func() { _ = appRuntime.closeMCP() })
	tui := &tuiBackend{runtime: appRuntime, manager: providerManager}
	assertSafeError("TUI", tui.MCPAction(context.Background(), "oauth", ui.MCPActionLogout))
}

func TestMCPEnableDisableUseConfigurationStore(t *testing.T) {
	for _, testCase := range []struct {
		verb    string
		enabled bool
	}{
		{verb: "enable", enabled: true},
		{verb: "disable", enabled: false},
	} {
		t.Run(testCase.verb, func(t *testing.T) {
			var gotName string
			var gotEnabled bool
			var stdout bytes.Buffer
			runtime := &runtime{stdout: &stdout, stderr: &bytes.Buffer{}, output: "json"}
			command := runtime.mcpCommandWithBackend(context.Background(), nil, func(_ context.Context, name string, enabled bool) error {
				gotName, gotEnabled = name, enabled
				return nil
			})
			command.SetArgs([]string{testCase.verb, "alpha"})
			if err := command.Execute(); err != nil {
				t.Fatalf("Execute() error=%v", err)
			}
			if gotName != "alpha" || gotEnabled != testCase.enabled {
				t.Fatalf("name=%q enabled=%t", gotName, gotEnabled)
			}
		})
	}
}

func TestMCPPromptRejectsNonStringArguments(t *testing.T) {
	runtime := &runtime{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, output: "json"}
	command := runtime.mcpCommandWithBackend(context.Background(), func(context.Context) (mcpCommandBackend, error) {
		return &fakeMCPCommandBackend{}, nil
	}, nil)
	command.SetArgs([]string{"prompt", "alpha", "review", "--arguments", `{"count":1}`})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "arguments") {
		t.Fatalf("error=%v", err)
	}
}

func TestMCPPromptCompletesArgumentNamesAndEventsRemainObservable(t *testing.T) {
	backend := &fakeMCPCommandBackend{events: []mcpclient.Event{{Kind: mcpclient.EventLogging, Server: "alpha", Message: "stored-secret log", Data: map[string]any{"level": "info"}}}}
	var stdout bytes.Buffer
	appRuntime := &runtime{stdout: &stdout, stderr: &bytes.Buffer{}, output: "json", apiKeys: []string{"stored-secret"}}
	command := appRuntime.mcpCommandWithBackend(context.Background(), func(context.Context) (mcpCommandBackend, error) { return backend, nil }, nil)
	var prompt *cobra.Command
	for _, child := range command.Commands() {
		if child.Name() == "prompt" {
			prompt = child
			break
		}
	}
	if prompt == nil || prompt.ValidArgsFunction == nil {
		t.Fatal("prompt argument completion is unavailable")
	}
	values, directive := prompt.ValidArgsFunction(prompt, []string{"alpha", "review"}, "lan")
	if len(values) != 1 || values[0] != "language=" || directive&cobra.ShellCompDirectiveNoSpace == 0 {
		t.Fatalf("completion values=%v directive=%v", values, directive)
	}
	backend.calls = nil
	command.SetArgs([]string{"events", "--wait", "50ms"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"kind":"logging"`) || strings.Contains(stdout.String(), "stored-secret") {
		t.Fatalf("events output=%q", stdout.String())
	}
}

func TestMCPListAndToggleConfiguredDisabledServer(t *testing.T) {
	workspace := t.TempDir()
	configPath := filepath.Join(workspace, "config.toml")
	configText := "version = 1\n\n[mcp_servers.alpha]\ntransport = \"stdio\"\nenabled = false\ncommand = \"missing-mcp-command\"\n"
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	args := []string{"--config", configPath, "--workspace", workspace, "--output", "json", "mcp", "list"}
	if code := Execute(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != exitOK {
		t.Fatalf("list code=%d stderr=%q", code, stderr.String())
	}
	var listed struct {
		Servers []mcpclient.ServerInfo `json:"servers"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v output=%q", err, stdout.String())
	}
	if len(listed.Servers) != 1 || listed.Servers[0].Name != "alpha" || listed.Servers[0].Status != "disabled" {
		t.Fatalf("servers=%#v", listed.Servers)
	}

	stdout.Reset()
	stderr.Reset()
	args[len(args)-1] = "enable"
	args = append(args, "alpha")
	if code := Execute(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != exitOK {
		t.Fatalf("enable code=%d stderr=%q", code, stderr.String())
	}
	updated, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "enabled = true") {
		t.Fatalf("enabled config=%q", updated)
	}

	stdout.Reset()
	stderr.Reset()
	args[len(args)-2] = "disable"
	if code := Execute(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != exitOK {
		t.Fatalf("disable code=%d stderr=%q", code, stderr.String())
	}
	updated, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "enabled = false") {
		t.Fatalf("disabled config=%q", updated)
	}
}
