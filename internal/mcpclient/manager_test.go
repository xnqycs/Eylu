package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkjsonrpc "github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"Eylu/internal/config"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("EYLU_MCP_HELPER") != "1" {
		return
	}
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "fixture-server", Version: "1.0.0"}, &sdkmcp.ServerOptions{Instructions: "Use fixture tools for deterministic tests."})
	type echoInput struct {
		Value string `json:"value"`
	}
	type echoOutput struct {
		Value string `json:"value"`
	}
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "echo", Description: "Echo a value", Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: true}}, func(_ context.Context, _ *sdkmcp.CallToolRequest, input echoInput) (*sdkmcp.CallToolResult, echoOutput, error) {
		return nil, echoOutput(input), nil
	})
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "hint_only", Description: "Annotation is only a hint", Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: true}}, func(_ context.Context, _ *sdkmcp.CallToolRequest, input echoInput) (*sdkmcp.CallToolResult, echoOutput, error) {
		return nil, echoOutput(input), nil
	})
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "wait", Description: "Wait for cancellation"}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ echoInput) (*sdkmcp.CallToolResult, echoOutput, error) {
		<-ctx.Done()
		return nil, echoOutput{}, ctx.Err()
	})
	if os.Getenv("EYLU_MCP_DYNAMIC") == "1" {
		sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "add_dynamic", Description: "Add a dynamic tool"}, func(_ context.Context, _ *sdkmcp.CallToolRequest, input echoInput) (*sdkmcp.CallToolResult, echoOutput, error) {
			sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "dynamic", Description: "Dynamically added"}, func(_ context.Context, _ *sdkmcp.CallToolRequest, value echoInput) (*sdkmcp.CallToolResult, echoOutput, error) {
				return nil, echoOutput(value), nil
			})
			return nil, echoOutput(input), nil
		})
	}
	server.AddResource(&sdkmcp.Resource{URI: "fixture://secret", Name: "secret", MIMEType: "text/plain"}, func(_ context.Context, _ *sdkmcp.ReadResourceRequest) (*sdkmcp.ReadResourceResult, error) {
		return &sdkmcp.ReadResourceResult{Contents: []*sdkmcp.ResourceContents{{URI: "fixture://secret", MIMEType: "text/plain", Text: "resource:" + os.Getenv("MCP_SECRET")}}}, nil
	})
	server.AddResourceTemplate(&sdkmcp.ResourceTemplate{URITemplate: "fixture://items/{id}", Name: "item"}, func(_ context.Context, request *sdkmcp.ReadResourceRequest) (*sdkmcp.ReadResourceResult, error) {
		return &sdkmcp.ReadResourceResult{Contents: []*sdkmcp.ResourceContents{{URI: request.Params.URI, MIMEType: "text/plain", Text: request.Params.URI}}}, nil
	})
	server.AddPrompt(&sdkmcp.Prompt{Name: "greet", Description: "Build a greeting", Arguments: []*sdkmcp.PromptArgument{{Name: "name", Required: true}}}, func(_ context.Context, request *sdkmcp.GetPromptRequest) (*sdkmcp.GetPromptResult, error) {
		return &sdkmcp.GetPromptResult{Messages: []*sdkmcp.PromptMessage{{Role: sdkmcp.Role("user"), Content: &sdkmcp.TextContent{Text: "hello " + request.Params.Arguments["name"]}}}}, nil
	})
	if err := server.Run(context.Background(), &sdkmcp.StdioTransport{}); err != nil {
		os.Exit(2)
	}
}

func TestProtocolErrorDataPreservesJSONRPCFields(t *testing.T) {
	rpcErr := &sdkjsonrpc.Error{Code: -32602, Message: "invalid params", Data: json.RawMessage(`{"field":"path"}`)}
	data := protocolErrorData(fmt.Errorf("call failed: %w", rpcErr))
	if data["code"] != int64(-32602) || data["message"] != "invalid params" || data["type"] != "jsonrpc" {
		t.Fatalf("protocol error data = %#v", data)
	}
	encoded, err := json.Marshal(data["data"])
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"field":"path"}` {
		t.Fatalf("protocol error data payload = %s", encoded)
	}
}

func TestInspectRedactsServerArgumentsAndURLs(t *testing.T) {
	secrets := []string{
		"long-inline-secret", "long-separate-secret", "short-separate-secret", "short-inline-secret",
		"combined-short-secret", "positional-secret", "after-terminator-secret",
		"url-user-secret", "url-password-secret", "url-query-secret", "url-tenant-secret", "url-fragment-secret",
		"issuer-user-secret", "issuer-password-secret", "issuer-query-secret",
		"redirect-user-secret", "redirect-password-secret", "redirect-query-secret",
	}
	cfg := config.MCPServerConfig{
		Command: "fixture-command",
		Args: []string{
			"--api-key=long-inline-secret",
			"--token", "long-separate-secret",
			"-k", "short-separate-secret",
			"-p=short-inline-secret",
			"-Dcombined-short-secret",
			"positional-secret",
			"--", "--after-terminator-secret",
		},
		URL: "https://url-user-secret:url-password-secret@example.test/mcp?api_key=url-query-secret&tenant=url-tenant-secret#url-fragment-secret",
		OAuth: &config.MCPOAuthConfig{
			Issuer:      "https://issuer-user-secret:issuer-password-secret@issuer.example.test/oauth?token=issuer-query-secret",
			ClientID:    "diagnostic-client",
			Scopes:      []string{"mcp:read"},
			RedirectURL: "https://redirect-user-secret:redirect-password-secret@localhost/callback?code=redirect-query-secret",
		},
	}
	manager := &Manager{servers: map[string]*serverRuntime{
		"fixture": {name: "fixture", config: cfg, status: StatusDisconnected},
	}}

	detail, err := manager.Inspect("fixture")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{
		"--api-key=[REDACTED]",
		"--token", "[REDACTED]",
		"-k", "[REDACTED]",
		"-p=[REDACTED]",
		"-D[REDACTED]",
		"[REDACTED]",
		"--", "[REDACTED]",
	}
	gotArgs, ok := detail.Config["args"].([]string)
	if !ok || strings.Join(gotArgs, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("redacted args = %#v, want %#v", gotArgs, wantArgs)
	}
	assertRedactedInspectURL(t, detail.Config["url"], "api_key", "tenant")
	oauth, ok := detail.Config["oauth"].(map[string]any)
	if !ok {
		t.Fatalf("oauth config = %#v", detail.Config["oauth"])
	}
	assertRedactedInspectURL(t, oauth["issuer"], "token")
	assertRedactedInspectURL(t, oauth["redirect_url"], "code")

	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf("%+v", detail)
	for _, output := range []string{string(encoded), text} {
		for _, secret := range secrets {
			if strings.Contains(output, secret) {
				t.Fatalf("Inspect output leaked %q: %s", secret, output)
			}
		}
		for _, diagnostic := range []string{"api_key", "tenant", "--api-key", "--token", "-k", "-p", "-D"} {
			if !strings.Contains(output, diagnostic) {
				t.Fatalf("Inspect output omitted diagnostic %q: %s", diagnostic, output)
			}
		}
	}
}

func assertRedactedInspectURL(t *testing.T, value any, queryKeys ...string) {
	t.Helper()
	redacted, ok := value.(string)
	if !ok {
		t.Fatalf("redacted URL = %#v", value)
	}
	parsed, err := url.Parse(redacted)
	if err != nil {
		t.Fatalf("parse redacted URL %q: %v", redacted, err)
	}
	if parsed.User != nil {
		t.Fatalf("redacted URL retained userinfo: %q", redacted)
	}
	for _, key := range queryKeys {
		values, exists := parsed.Query()[key]
		if !exists || len(values) == 0 {
			t.Fatalf("redacted URL omitted query key %q: %q", key, redacted)
		}
		for _, value := range values {
			if value != "[REDACTED]" {
				t.Fatalf("redacted URL query %q = %q", key, value)
			}
		}
	}
	if parsed.Fragment != "[REDACTED]" && parsed.Fragment != "" {
		t.Fatalf("redacted URL fragment = %q", parsed.Fragment)
	}
}

func TestManagerConnectsToolsResourcesAndPolicy(t *testing.T) {
	t.Setenv("EYLU_MCP_HELPER", "1")
	t.Setenv("MCP_SECRET", "forwarded")
	workspace := t.TempDir()
	serverConfig := config.MCPServerConfig{
		Command: os.Args[0], Args: []string{"-test.run=^TestMCPHelperProcess$"},
		Environment: []string{"EYLU_MCP_HELPER", "MCP_SECRET"}, ReadOnlyTools: []string{"echo"}, TimeoutSeconds: 5,
	}
	manager, diagnostics, err := Open(context.Background(), map[string]config.MCPServerConfig{"fixture": serverConfig}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if len(diagnostics) != 0 || len(manager.Servers()) != 1 || len(manager.Contexts()) != 1 || manager.Fingerprint() == "" {
		t.Fatalf("servers=%#v contexts=%#v diagnostics=%#v", manager.Servers(), manager.Contexts(), diagnostics)
	}
	serverContext := manager.Contexts()[0]
	if !strings.Contains(serverContext.Instructions, "deterministic") || !strings.Contains(serverContext.ResourceCatalog, "fixture://secret") || len(serverContext.ToolDefinitions) != 4 {
		t.Fatalf("context = %#v", serverContext)
	}
	detail, err := manager.Inspect("fixture")
	if err != nil || detail.Status != StatusConnected || len(detail.ResourceTemplates) != 1 || len(detail.Prompts) != 1 || len(detail.Resources) != 1 {
		t.Fatalf("detail=%#v error=%v", detail, err)
	}
	prompt, err := manager.GetPrompt(context.Background(), "fixture", "greet", map[string]string{"name": "Eylu"})
	if err != nil || len(prompt.Messages) != 1 {
		t.Fatalf("prompt=%#v error=%v", prompt, err)
	}
	if text, ok := prompt.Messages[0].Content.(*sdkmcp.TextContent); !ok || text.Text != "hello Eylu" {
		t.Fatalf("prompt content=%#v", prompt.Messages[0].Content)
	}
	registry := tool.NewRegistry(manager.Tools()...)
	echo, ok := registry.Get("mcp__fixture__echo")
	if !ok || echo.Risk() != policy.RiskRead || !echo.(tool.ParallelSafe).ParallelSafe() {
		t.Fatalf("echo tool = %#v", echo)
	}
	if spec := echo.(tool.ConcurrencyClassifier).ClassifyConcurrency(nil, policy.Outcome{}); spec.Mode != tool.ConcurrencyShared {
		t.Fatalf("echo concurrency = %#v", spec)
	}
	hintOnly, ok := registry.Get("mcp__fixture__hint_only")
	if !ok || hintOnly.Risk() != policy.RiskWrite {
		t.Fatalf("hint-only risk = %v", hintOnly.Risk())
	}
	if safe, ok := hintOnly.(tool.ParallelSafe); !ok || safe.ParallelSafe() {
		t.Fatal("untrusted MCP annotation granted parallel read authorization")
	}
	if spec := hintOnly.(tool.ConcurrencyClassifier).ClassifyConcurrency(nil, policy.Outcome{}); spec.Mode != tool.ConcurrencyExclusive {
		t.Fatalf("hint-only concurrency = %#v", spec)
	}

	executor := &tool.Executor{Registry: registry, Policy: policy.AllowAllChecker{}, MaxParallelTools: 2, Timeout: time.Second}
	calls := []protocol.ToolCall{
		{ID: "one", Name: "mcp__fixture__echo", Arguments: json.RawMessage(`{"value":"one"}`)},
		{ID: "two", Name: "mcp__fixture__echo", Arguments: json.RawMessage(`{"value":"two"}`)},
	}
	results := executor.ExecuteConcurrent(context.Background(), "request", calls)
	if len(results) != 2 || results[0].CallID != "one" || !strings.Contains(results[0].Content, "one") || results[1].CallID != "two" || !strings.Contains(results[1].Content, "two") {
		t.Fatalf("results = %#v", results)
	}
	resource, ok := registry.Get("mcp__fixture__read_resource")
	if !ok {
		t.Fatal("resource tool missing")
	}
	if spec := resource.(tool.ConcurrencyClassifier).ClassifyConcurrency(nil, policy.Outcome{}); spec.Mode != tool.ConcurrencyShared {
		t.Fatalf("resource concurrency = %#v", spec)
	}
	resourceResult := resource.Execute(context.Background(), json.RawMessage(`{"uri":"fixture://secret"}`))
	if resourceResult.IsError || resourceResult.Content != "resource:forwarded" || resourceResult.Metadata["mcp_server"] != "fixture" {
		t.Fatalf("resource result = %#v", resourceResult)
	}

	waitTool, ok := registry.Get("mcp__fixture__wait")
	if !ok {
		t.Fatal("wait tool missing")
	}
	waitContext, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	waitResult := waitTool.Execute(waitContext, json.RawMessage(`{"value":""}`))
	if !waitResult.IsError || !strings.Contains(strings.ToLower(waitResult.Content), "cancel") && !strings.Contains(strings.ToLower(waitResult.Content), "deadline") {
		t.Fatalf("wait result = %#v", waitResult)
	}
}

func TestManagerFiltersToolsAndListsDisabledServers(t *testing.T) {
	t.Setenv("EYLU_MCP_HELPER", "1")
	workspace := t.TempDir()
	manager, diagnostics, err := Open(context.Background(), map[string]config.MCPServerConfig{
		"disabled": {Command: "unused", Disabled: true},
		"fixture": {
			Command: os.Args[0], Args: []string{"-test.run=^TestMCPHelperProcess$"}, Environment: []string{"EYLU_MCP_HELPER"},
			AllowTools: []string{"echo", "wait"}, DenyTools: []string{"wait"}, StartupTimeoutSeconds: 5,
		},
	}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
	listed := manager.List()
	if len(listed) != 2 || listed[0].Name != "disabled" || listed[0].Status != StatusDisabled || listed[1].Status != StatusConnected {
		t.Fatalf("listed=%#v", listed)
	}
	tools, err := manager.ServerTools("fixture")
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools=%#v error=%v", tools, err)
	}
	if _, err := manager.Tool("fixture", "wait"); err == nil {
		t.Fatal("denied tool remained visible")
	}
}

func TestManagerAppliesConfiguredCallTimeout(t *testing.T) {
	t.Setenv("EYLU_MCP_HELPER", "1")
	manager, _, err := Open(context.Background(), map[string]config.MCPServerConfig{
		"fixture": {
			Command: os.Args[0], Args: []string{"-test.run=^TestMCPHelperProcess$"}, Environment: []string{"EYLU_MCP_HELPER"},
			StartupTimeoutSeconds: 5, CallTimeoutSeconds: 1,
		},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	waitTool, ok := tool.NewRegistry(manager.Tools()...).Get("mcp__fixture__wait")
	if !ok {
		t.Fatal("wait tool missing")
	}
	started := time.Now()
	result := waitTool.Execute(context.Background(), json.RawMessage(`{"value":""}`))
	elapsed := time.Since(started)
	if !result.IsError || !strings.Contains(strings.ToLower(result.Content), "deadline") && !strings.Contains(strings.ToLower(result.Content), "cancel") {
		t.Fatalf("wait result = %#v", result)
	}
	if elapsed < 750*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("configured call timeout elapsed = %s", elapsed)
	}
}

func TestManagerRequiredFailureStopsStartup(t *testing.T) {
	manager, diagnostics, err := Open(context.Background(), map[string]config.MCPServerConfig{
		"required": {Command: os.Args[0], WorkingDirectory: "..", Required: true},
	}, t.TempDir())
	if err == nil || manager != nil || len(diagnostics) != 1 || !strings.Contains(err.Error(), "required MCP server") {
		t.Fatalf("manager=%#v diagnostics=%#v error=%v", manager, diagnostics, err)
	}
}

func TestManagerDisableCancelsCallsRemovesCatalogAndEmitsStatus(t *testing.T) {
	t.Setenv("EYLU_MCP_HELPER", "1")
	manager, _, err := Open(context.Background(), map[string]config.MCPServerConfig{
		"fixture": {Command: os.Args[0], Args: []string{"-test.run=^TestMCPHelperProcess$"}, Environment: []string{"EYLU_MCP_HELPER"}, StartupTimeoutSeconds: 5},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	events, unsubscribe := manager.SubscribeEvents(8)
	defer unsubscribe()
	registry := tool.NewRegistry(manager.Tools()...)
	waitTool, ok := registry.Get("mcp__fixture__wait")
	if !ok {
		t.Fatal("wait tool missing")
	}
	result := make(chan protocol.ToolResult, 1)
	go func() { result <- waitTool.Execute(context.Background(), json.RawMessage(`{"value":""}`)) }()
	time.Sleep(100 * time.Millisecond)
	if err := manager.Disable(context.Background(), "fixture"); err != nil {
		t.Fatal(err)
	}
	select {
	case callResult := <-result:
		if !callResult.IsError {
			t.Fatalf("call result=%#v", callResult)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("disable did not cancel an in-flight call")
	}
	if len(manager.Tools()) != 0 || len(manager.Contexts()) != 0 || manager.List()[0].Status != StatusDisabled {
		t.Fatalf("tools=%d contexts=%d list=%#v", len(manager.Tools()), len(manager.Contexts()), manager.List())
	}
	select {
	case event := <-events:
		if event.Kind != EventStatus || event.Status != StatusDisabled {
			t.Fatalf("event=%#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("disable status event was not emitted")
	}
}

func TestManagerListChangedAtomicallyRefreshesCatalog(t *testing.T) {
	t.Setenv("EYLU_MCP_HELPER", "1")
	t.Setenv("EYLU_MCP_DYNAMIC", "1")
	manager, _, err := Open(context.Background(), map[string]config.MCPServerConfig{
		"fixture": {
			Command: os.Args[0], Args: []string{"-test.run=^TestMCPHelperProcess$"},
			Environment: []string{"EYLU_MCP_HELPER", "EYLU_MCP_DYNAMIC"}, StartupTimeoutSeconds: 5,
		},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	before := manager.Fingerprint()
	registry := tool.NewRegistry(manager.Tools()...)
	add, ok := registry.Get("mcp__fixture__add_dynamic")
	if !ok {
		t.Fatal("dynamic trigger tool missing")
	}
	if result := add.Execute(context.Background(), json.RawMessage(`{"value":"go"}`)); result.IsError {
		t.Fatalf("trigger result=%#v", result)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		tools, listErr := manager.ServerTools("fixture")
		if listErr != nil {
			t.Fatal(listErr)
		}
		for _, candidate := range tools {
			if candidate.Name == "dynamic" {
				if manager.Fingerprint() == before {
					t.Fatal("catalog changed without updating the fingerprint")
				}
				if _, exists := tool.NewRegistry(manager.Tools()...).Get("mcp__fixture__dynamic"); !exists {
					t.Fatal("catalog DTO and tool registry were not replaced atomically")
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("listChanged did not refresh the catalog")
}

func TestManagerReportsEscapingWorkingDirectory(t *testing.T) {
	manager, diagnostics, err := Open(context.Background(), map[string]config.MCPServerConfig{
		"escape": {Command: os.Args[0], WorkingDirectory: ".."},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0].Message, "escapes") || len(manager.Servers()) != 0 {
		t.Fatalf("diagnostics=%#v servers=%#v", diagnostics, manager.Servers())
	}
}

func TestLocalToolNameIsStableASCIIAndBounded(t *testing.T) {
	first := localToolName(strings.Repeat("server", 20), strings.Repeat("工具/", 40)+"one")
	second := localToolName(strings.Repeat("server", 20), strings.Repeat("工具/", 40)+"two")
	if len(first) > maxLocalToolNameBytes || first == second || first != localToolName(strings.Repeat("server", 20), strings.Repeat("工具/", 40)+"one") {
		t.Fatalf("first=%q second=%q", first, second)
	}
	if localToolName("server", "工具") == localToolName("server", "测试") {
		t.Fatal("distinct Unicode tool names collided")
	}
	for _, value := range []byte(first + second) {
		if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '_' || value == '-' {
			continue
		}
		t.Fatalf("tool name contains non-ASCII-safe byte %q", value)
	}
}

func TestResolveWorkingDirectoryRejectsSymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(workspace, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := resolveWorkingDirectory(workspace, "linked"); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("error = %v", err)
	}
}
