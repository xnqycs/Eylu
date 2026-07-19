package mcpclient

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	server.AddResource(&sdkmcp.Resource{URI: "fixture://secret", Name: "secret", MIMEType: "text/plain"}, func(_ context.Context, _ *sdkmcp.ReadResourceRequest) (*sdkmcp.ReadResourceResult, error) {
		return &sdkmcp.ReadResourceResult{Contents: []*sdkmcp.ResourceContents{{URI: "fixture://secret", MIMEType: "text/plain", Text: "resource:" + os.Getenv("MCP_SECRET")}}}, nil
	})
	if err := server.Run(context.Background(), &sdkmcp.StdioTransport{}); err != nil {
		os.Exit(2)
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
	registry := tool.NewRegistry(manager.Tools()...)
	echo, ok := registry.Get("mcp__fixture__echo")
	if !ok || echo.Risk() != policy.RiskRead || !echo.(tool.ParallelSafe).ParallelSafe() {
		t.Fatalf("echo tool = %#v", echo)
	}
	hintOnly, ok := registry.Get("mcp__fixture__hint_only")
	if !ok || hintOnly.Risk() != policy.RiskWrite {
		t.Fatalf("hint-only risk = %v", hintOnly.Risk())
	}
	if safe, ok := hintOnly.(tool.ParallelSafe); !ok || safe.ParallelSafe() {
		t.Fatal("untrusted MCP annotation granted parallel read authorization")
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
