package tool

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type fakeTool struct {
	name   string
	risk   policy.Risk
	result protocol.ToolResult
	block  bool
	calls  int
}

func (t *fakeTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: t.name, Description: "fake", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (t *fakeTool) Risk() policy.Risk { return t.risk }
func (t *fakeTool) Execute(ctx context.Context, _ json.RawMessage) protocol.ToolResult {
	t.calls++
	if t.block {
		<-ctx.Done()
		return toolError(ctx.Err().Error())
	}
	return t.result
}

type memoryAudit struct {
	mu      sync.Mutex
	records []AuditRecord
}

func (a *memoryAudit) Record(record AuditRecord) {
	a.mu.Lock()
	a.records = append(a.records, record)
	a.mu.Unlock()
}

func TestRegistryAndExecutor(t *testing.T) {
	read := &fakeTool{name: "read", risk: policy.RiskRead, result: protocol.ToolResult{Content: "ok"}}
	write := &fakeTool{name: "write", risk: policy.RiskWrite, result: protocol.ToolResult{Content: strings.Repeat("界", 20)}}
	registry := NewRegistry(read, write)
	if got := registry.Definitions(); len(got) != 2 || got[0].Name != "read" || got[1].Name != "write" {
		t.Fatalf("definitions = %#v", got)
	}
	if err := registry.Register(read); err == nil {
		t.Fatal("expected duplicate registration error")
	}
	audit := &memoryAudit{}
	executor := &Executor{Registry: registry, Policy: policy.BaselineChecker{}, Workspace: t.TempDir(), MaxOutputBytes: 20, Audit: audit}
	result := executor.Execute(context.Background(), "request", protocol.ToolCall{ID: "1", Name: "read", Arguments: json.RawMessage(`{}`)})
	if result.IsError || result.Content != "ok" || read.calls != 1 {
		t.Fatalf("read result = %#v", result)
	}
	result = executor.Execute(context.Background(), "request", protocol.ToolCall{ID: "2", Name: "write", Arguments: json.RawMessage(`{}`)})
	if !result.IsError || !strings.Contains(result.Content, "confirmation required") || write.calls != 0 {
		t.Fatalf("unconfirmed write = %#v", result)
	}
	executor.Confirm = func(context.Context, policy.Request, policy.Outcome) (Confirmation, error) {
		return Confirmation{Approved: true}, nil
	}
	result = executor.Execute(context.Background(), "request", protocol.ToolCall{ID: "3", Name: "write", Arguments: json.RawMessage(`{}`)})
	if result.IsError || !result.Truncated || !strings.Contains(result.Content, "[output truncated]") || len([]byte(result.Content)) > 20 {
		t.Fatalf("confirmed write = %#v", result)
	}
	result = executor.Execute(context.Background(), "request", protocol.ToolCall{ID: "4", Name: "missing", Arguments: json.RawMessage(`{}`)})
	if !result.IsError || !strings.Contains(result.Content, "unknown tool") {
		t.Fatalf("unknown result = %#v", result)
	}
	result = executor.Execute(context.Background(), "request", protocol.ToolCall{ID: "5", Name: "read", Arguments: json.RawMessage(`{"broken"`)})
	if !result.IsError || !strings.Contains(result.Content, "invalid JSON") {
		t.Fatalf("bad JSON result = %#v", result)
	}
	if len(audit.records) != 5 || audit.records[2].Decision != policy.DecisionConfirm || !audit.records[2].Confirmed {
		t.Fatalf("audit = %#v", audit.records)
	}
}

func TestExecutorTimeout(t *testing.T) {
	blocked := &fakeTool{name: "blocked", risk: policy.RiskRead, block: true}
	executor := &Executor{Registry: NewRegistry(blocked), Policy: policy.AllowAllChecker{}, Timeout: 5 * time.Millisecond}
	result := executor.Execute(context.Background(), "request", protocol.ToolCall{ID: "1", Name: "blocked", Arguments: json.RawMessage(`{}`)})
	if !result.IsError || result.Content != "tool execution timed out" {
		t.Fatalf("result = %#v", result)
	}
}

func TestDecodeStrictRejectsUnknownAndTrailing(t *testing.T) {
	var target struct {
		Name string `json:"name"`
	}
	for _, input := range []string{`{"name":"ok","extra":true}`, `{"name":"ok"} {"name":"again"}`} {
		if err := decodeStrict(json.RawMessage(input), &target); err == nil {
			t.Fatalf("input %q should fail", input)
		}
	}
}
