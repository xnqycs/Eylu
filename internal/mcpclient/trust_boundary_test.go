package mcpclient

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"Eylu/internal/config"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

// stubTool is an ordinary host tool, used to stand in for a built-in that a
// server might try to shadow.
type stubTool struct{ name string }

func (t stubTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: t.name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (t stubTool) Risk() policy.Risk { return policy.RiskRead }
func (t stubTool) Execute(context.Context, json.RawMessage) protocol.ToolResult {
	return protocol.ToolResult{Content: "stub"}
}

// openFixtureServer starts the in-process fixture server and waits until its
// catalog has been fetched.
func openFixtureServer(t *testing.T, serverConfig config.MCPServerConfig) *Manager {
	t.Helper()
	t.Setenv("EYLU_MCP_HELPER", "1")
	manager, diagnostics, err := Open(context.Background(), map[string]config.MCPServerConfig{"fixture": serverConfig}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		contexts := manager.Contexts()
		if len(contexts) == 1 && len(contexts[0].ToolDefinitions) == 4 {
			return manager
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fixture catalog never arrived: %#v", contexts)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fixtureServerConfig(readOnlyTools ...string) config.MCPServerConfig {
	return config.MCPServerConfig{
		Command: os.Args[0], Args: []string{"-test.run=^TestMCPHelperProcess$"},
		Environment: []string{"EYLU_MCP_HELPER"}, ReadOnlyTools: readOnlyTools, StartupTimeoutSeconds: 5,
	}
}

// T-01: a server cannot declare its own tool read-only.
//
// The fixture advertises both `echo` and `hint_only` with ReadOnlyHint set. Only
// the name listed in the local configuration may be treated as a read: the hint
// is a claim by the server, and a server that can talk the model into calling it
// gains nothing by claiming to be harmless.
func TestMCPServerCannotDeclareItsOwnToolReadOnly(t *testing.T) {
	manager := openFixtureServer(t, fixtureServerConfig("echo"))
	registry := tool.NewRegistry(manager.Tools()...)

	advertised, ok := registry.Get("mcp__fixture__hint_only")
	if !ok {
		t.Fatal("the fixture tool hint_only is missing")
	}
	// The fixture does declare ReadOnlyHint on this tool; the local configuration
	// does not list it, so it stays a write.
	if advertised.Risk() != policy.RiskWrite {
		t.Fatalf("a server's own read-only claim was trusted: risk = %v", advertised.Risk())
	}
	if safe, ok := advertised.(tool.ParallelSafe); !ok || safe.ParallelSafe() {
		t.Fatal("a server's own read-only claim made its tool run in parallel with other work")
	}
	if spec := advertised.(tool.ConcurrencyClassifier).ClassifyConcurrency(nil, policy.Outcome{}); spec.Mode != tool.ConcurrencyExclusive {
		t.Fatalf("concurrency = %#v, want exclusive", spec)
	}
	// The declaration in the catalog says "write" as well, so an operator reading
	// the server list is not told the opposite of what the executor does.
	detail, err := manager.Inspect("fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range detail.Tools {
		if info.Name == "hint_only" && info.Permission != "write" {
			t.Fatalf("hint_only permission = %q, want write", info.Permission)
		}
		if info.Name == "echo" && info.Permission != "read" {
			t.Fatalf("echo permission = %q, want read", info.Permission)
		}
	}

	// The other direction: a tool the local configuration does list is a read,
	// whatever the server says about it.
	granted, ok := registry.Get("mcp__fixture__echo")
	if !ok {
		t.Fatal("the fixture tool echo is missing")
	}
	if granted.Risk() != policy.RiskRead {
		t.Fatalf("a locally granted read was not honoured: risk = %v", granted.Risk())
	}
}

// T-02: an MCP tool name always carries its server, and a name that is already
// taken is refused rather than silently replacing the tool that held it.
func TestMCPToolNamesCannotCollideWithBuiltinTools(t *testing.T) {
	manager := openFixtureServer(t, fixtureServerConfig())
	provided := manager.Tools()
	if len(provided) == 0 {
		t.Fatal("the fixture server provided no tools")
	}
	for _, item := range provided {
		name := item.Definition().Name
		if !strings.HasPrefix(name, "mcp__fixture__") {
			t.Fatalf("an MCP tool reached the registry without its server prefix: %q", name)
		}
	}

	// A built-in that already holds the prefixed name cannot be replaced by the
	// server's tool.
	squatted := "mcp__fixture__echo"
	host := tool.NewRegistry(stubTool{name: squatted})
	var collision error
	for _, item := range provided {
		if err := host.Register(item); err != nil && item.Definition().Name == squatted {
			collision = err
		}
	}
	if collision == nil {
		t.Fatal("registering a server tool over an existing tool was accepted")
	}
	kept, ok := host.Get(squatted)
	if !ok {
		t.Fatal("the tool that held the name first disappeared")
	}
	if _, isStub := kept.(stubTool); !isStub {
		t.Fatalf("the existing tool was overwritten by %T", kept)
	}

	// And the reverse order: once the server's tool is registered, a later
	// built-in with the same name is refused too.
	host = tool.NewRegistry(provided...)
	if err := host.Register(stubTool{name: squatted}); err == nil {
		t.Fatal("a built-in was allowed to overwrite a server tool")
	}
	if item, ok := host.Get(squatted); !ok {
		t.Fatal("the server tool disappeared")
	} else if _, isStub := item.(stubTool); isStub {
		t.Fatal("the server tool was replaced by the built-in")
	}
}
