package tool

import (
	"context"
	"encoding/json"
	"testing"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

// overrideTool is a fake tool that also supplies a tool-level policy decision.
type overrideTool struct {
	fakeTool
	outcome policy.Outcome
	applied bool
}

func (t *overrideTool) OverridePolicy(json.RawMessage) (policy.Outcome, bool) {
	return t.outcome, t.applied
}

// A tool-level allow must not relax an explicit operator prohibition, and the
// audit record must show which layer decided.
func TestExecutorToolOverrideCannotRelaxHardDeny(t *testing.T) {
	item := &overrideTool{
		fakeTool: fakeTool{name: "bash", risk: policy.RiskExec, result: protocol.ToolResult{Content: "executed"}},
		outcome:  policy.Outcome{Decision: policy.DecisionAllow, Source: policy.SourceTool, Rule: "tool_domain"},
		applied:  true,
	}
	config := policy.DefaultConfig(policy.ModeFull)
	config.BlockedPatterns = []string{"curl"}
	audit := &memoryAudit{}
	executor := &Executor{Registry: NewRegistry(item), Policy: policy.NewChecker(config), Audit: audit}
	result := executor.Execute(context.Background(), "request", protocol.ToolCall{
		ID: "blocked", Name: "bash", Arguments: json.RawMessage(`{"command":"curl https://example.com"}`),
	})
	if !result.IsError || item.calls != 0 {
		t.Fatalf("result = %#v, calls = %d", result, item.calls)
	}
	if len(audit.records) != 1 {
		t.Fatalf("audit = %#v", audit.records)
	}
	record := audit.records[0]
	if record.Decision != policy.DecisionDeny || record.PolicyRule != "blocked_pattern" || record.PolicySource != string(policy.SourceWorkspace) {
		t.Fatalf("audit record = %#v", record)
	}
	if record.PolicyOverride != "tool:tool_domain" {
		t.Fatalf("override record = %q", record.PolicyOverride)
	}
}

// An unrecognized decision fails closed: the tool must not execute.
func TestExecutorDeniesUnrecognizedDecision(t *testing.T) {
	item := &overrideTool{
		fakeTool: fakeTool{name: "edit_file", risk: policy.RiskWrite, result: protocol.ToolResult{Content: "written"}},
		outcome:  policy.Outcome{Decision: policy.Decision("restricted")},
		applied:  true,
	}
	audit := &memoryAudit{}
	executor := &Executor{Registry: NewRegistry(item), Policy: policy.NewChecker(policy.DefaultConfig(policy.ModeFull)), Audit: audit}
	result := executor.Execute(context.Background(), "request", protocol.ToolCall{ID: "write", Name: "edit_file", Arguments: json.RawMessage(`{}`)})
	if !result.IsError || item.calls != 0 {
		t.Fatalf("result = %#v, calls = %d", result, item.calls)
	}
	if len(audit.records) != 1 || audit.records[0].Decision != policy.DecisionDeny || audit.records[0].PolicyRule != "unrecognized_decision" {
		t.Fatalf("audit = %#v", audit.records)
	}
}

// A granted domain decision still applies, but the audit keeps the session mode
// from the workspace layer instead of the zero value of the tool outcome.
func TestExecutorDomainOverrideKeepsWorkspaceMode(t *testing.T) {
	item := &overrideTool{
		fakeTool: fakeTool{name: "edit_file", risk: policy.RiskWrite, result: protocol.ToolResult{Content: "written"}},
		outcome:  policy.Outcome{Decision: policy.DecisionAllow, Source: policy.SourceTool, Rule: "web_permission:allow"},
		applied:  true,
	}
	audit := &memoryAudit{}
	executor := &Executor{Registry: NewRegistry(item), Policy: policy.NewChecker(policy.DefaultConfig(policy.ModePlan)), Audit: audit}
	result := executor.Execute(context.Background(), "request", protocol.ToolCall{ID: "write", Name: "edit_file", Arguments: json.RawMessage(`{}`)})
	if result.IsError || item.calls != 1 {
		t.Fatalf("result = %#v, calls = %d", result, item.calls)
	}
	if len(audit.records) != 1 || audit.records[0].Mode != "plan" || audit.records[0].PolicySource != string(policy.SourceTool) {
		t.Fatalf("audit = %#v", audit.records)
	}
}

// Side-effecting command arguments must require confirmation instead of being
// auto-approved through a read-only prefix.
func TestExecutorRequiresConfirmationForSideEffectingArguments(t *testing.T) {
	item := &fakeTool{name: "bash", risk: policy.RiskExec, result: protocol.ToolResult{Content: "executed"}}
	confirmations := 0
	executor := &Executor{
		Registry: NewRegistry(item), Policy: policy.NewChecker(policy.DefaultConfig(policy.ModeAuto)),
		Confirm: func(_ context.Context, _ policy.Request, outcome policy.Outcome) (Confirmation, error) {
			confirmations++
			if outcome.Classification != policy.CommandDangerous || !outcome.Warning {
				t.Errorf("outcome = %#v", outcome)
			}
			return Confirmation{Approved: true}, nil
		},
	}
	result := executor.Execute(context.Background(), "request", protocol.ToolCall{ID: "find", Name: "bash", Arguments: json.RawMessage(`{"command":"find . -delete"}`)})
	if result.IsError || item.calls != 1 || confirmations != 2 {
		t.Fatalf("result = %#v calls = %d confirmations = %d", result, item.calls, confirmations)
	}
}
