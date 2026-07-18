package tool

import (
	"context"
	"encoding/json"
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
		Confirm: func(_ context.Context, request policy.Request, outcome policy.Outcome) (bool, error) {
			steps = append(steps, request.ConfirmationStep)
			if request.ConfirmationTotal != 2 || !outcome.Warning {
				t.Fatalf("request = %#v, outcome = %#v", request, outcome)
			}
			return true, nil
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
		Confirm: func(_ context.Context, request policy.Request, _ policy.Outcome) (bool, error) {
			return request.ConfirmationStep == 1, nil
		},
	}
	result := executor.Execute(context.Background(), "request", protocol.ToolCall{ID: "danger", Name: "bash", Arguments: json.RawMessage(`{"command":"git reset --hard HEAD"}`)})
	if !result.IsError || bash.calls != 0 || result.Content != "confirmation rejected at step 2 of 2" {
		t.Fatalf("result = %#v, calls = %d", result, bash.calls)
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
