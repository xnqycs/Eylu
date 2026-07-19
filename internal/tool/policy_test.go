package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

func TestExecutorPerformsAllDangerousConfirmations(t *testing.T) {
	bash := &fakeTool{name: "bash", risk: policy.RiskExec, result: protocol.ToolResult{Content: "executed"}}
	audit := &memoryAudit{}
	steps := make([]int, 0)
	executor := &Executor{
		Registry: NewRegistry(bash),
		Policy:   policy.NewChecker(policy.DefaultConfig(policy.ModeManual)),
		Audit:    audit,
		Confirm: func(_ context.Context, request policy.Request, outcome policy.Outcome) (Confirmation, error) {
			steps = append(steps, request.ConfirmationStep)
			if request.ConfirmationTotal != 2 || !outcome.Warning {
				t.Fatalf("request = %#v, outcome = %#v", request, outcome)
			}
			return Confirmation{Approved: true}, nil
		},
	}
	result := executor.Execute(context.Background(), "request", protocol.ToolCall{ID: "danger", Name: "bash", Arguments: json.RawMessage(`{"command":"rm -rf build"}`)})
	if result.IsError || bash.calls != 1 || len(steps) != 2 || steps[0] != 1 || steps[1] != 2 {
		t.Fatalf("result=%#v calls=%d steps=%#v", result, bash.calls, steps)
	}
	if len(audit.records) != 1 || audit.records[0].Mode != "manual" || audit.records[0].Classification != policy.CommandDangerous || audit.records[0].Confirmations != 2 || !audit.records[0].Warning {
		t.Fatalf("audit = %#v", audit.records)
	}
}

func TestExecutorStopsAtRejectedConfirmation(t *testing.T) {
	bash := &fakeTool{name: "bash", risk: policy.RiskExec, result: protocol.ToolResult{Content: "executed"}}
	executor := &Executor{
		Registry: NewRegistry(bash), Policy: policy.NewChecker(policy.DefaultConfig(policy.ModeAuto)),
		Confirm: func(_ context.Context, request policy.Request, _ policy.Outcome) (Confirmation, error) {
			return Confirmation{Approved: request.ConfirmationStep == 1}, nil
		},
	}
	result := executor.Execute(context.Background(), "request", protocol.ToolCall{ID: "danger", Name: "bash", Arguments: json.RawMessage(`{"command":"git reset --hard HEAD"}`)})
	if !result.IsError || bash.calls != 0 || result.Content != "approval rejected" || result.Metadata["interrupt_request"] != true {
		t.Fatalf("result = %#v, calls = %d", result, bash.calls)
	}
}

func TestExecutorReturnsModelVisibleRejectionReason(t *testing.T) {
	write := &fakeTool{name: "write_file", risk: policy.RiskWrite, result: protocol.ToolResult{Content: "written"}}
	executor := &Executor{
		Registry: NewRegistry(write), Policy: policy.NewChecker(policy.DefaultConfig(policy.ModeManual)),
		Confirm: func(context.Context, policy.Request, policy.Outcome) (Confirmation, error) {
			return Confirmation{RejectionReason: "Keep the public API unchanged"}, nil
		},
	}
	result := executor.Execute(context.Background(), "request", protocol.ToolCall{ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"path":"x","reason":"Update the implementation"}`)})
	if !result.IsError || write.calls != 0 || !strings.Contains(result.Content, "Keep the public API unchanged") || result.Metadata["interrupt_request"] == true {
		t.Fatalf("result=%#v calls=%d", result, write.calls)
	}
}

func TestApprovalToolSchemasRequireModelReason(t *testing.T) {
	workspace := t.TempDir()
	bash, err := NewBash(workspace, 1024, helperShell{})
	if err != nil {
		t.Fatal(err)
	}
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	edit, err := NewEditFile(workspace, 1024)
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range []protocol.ToolDefinition{bash.Definition(), write.Definition(), edit.Definition()} {
		if schema := string(definition.InputSchema); !strings.Contains(schema, `"reason"`) || !strings.Contains(schema, `"required"`) {
			t.Fatalf("%s schema = %s", definition.Name, schema)
		}
	}
	custom := &fakeTool{name: "mcp__demo__write", risk: policy.RiskWrite}
	definitions := (&Executor{Registry: NewRegistry(custom)}).Definitions()
	if len(definitions) != 1 || !strings.Contains(string(definitions[0].InputSchema), `"reason"`) {
		t.Fatalf("augmented definition = %#v", definitions)
	}
}

func TestPlanModeDenialBecomesToolResult(t *testing.T) {
	write := &fakeTool{name: "write_file", risk: policy.RiskWrite, result: protocol.ToolResult{Content: "written"}}
	executor := &Executor{Registry: NewRegistry(write), Policy: policy.NewChecker(policy.DefaultConfig(policy.ModePlan))}
	result := executor.Execute(context.Background(), "request", protocol.ToolCall{ID: "write", Name: "write_file", Arguments: json.RawMessage(`{}`)})
	if !result.IsError || write.calls != 0 || result.Content != "permission denied: plan mode permits exploration and read-only commands" {
		t.Fatalf("result = %#v", result)
	}
}
