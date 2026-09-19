package agent

import (
	"context"
	"path/filepath"
	"testing"

	"Eylu/internal/docscheck"
	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

// TestRunReportKeepsItsCommittedFieldContract is the machine-checkable half of
// the run report's promise.
//
// The report is written for a program that parses it without being able to ask
// which build produced it, which is why it carries a schema_version. This test is
// what keeps that version honest: a field removed or renamed fails here, and a
// field added has to be recorded in the contract in the same change.
func TestRunReportKeepsItsCommittedFieldContract(t *testing.T) {
	docscheck.CheckFieldContract(t, filepath.Join("testdata", "run_report_format.txt"), RunReport{
		SchemaVersion: RunReportSchemaVersion,
		RequestID:     "request", Iterations: 2, StopReason: string(protocol.StopCompleted), Error: "failure",
		ModelCalls: 3, ToolCalls: 4, Succeeded: 1, Failed: 1, Rejected: 1, Cancelled: 1, NotExecuted: 1, OutcomeUnknown: 1,
		InputTokens: 10, OutputTokens: 20, ReasoningTokens: 30, ExactUsage: true, EstimatedUsage: true, CachedInputTokens: 5,
		AuditFailures: 1, EventsDropped: 2, Warnings: []string{"warning"}, PendingAtEnd: 1,
		RecoveredCalls: []string{"call"}, Interop: []string{"relaxation"},
	})
}

// A report the request itself produced carries its version, so the field is not
// merely declared on the struct: the code that starts a request is what stamps
// it.
func TestAFinishedRequestNamesItsReportSchemaVersion(t *testing.T) {
	model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		return textResponse("agent-1", "answer"), nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(), Policy: policy.AllowAllChecker{}}
	report := RunReport{}
	if _, err := NewConversation().Run(context.Background(), "report", testRuntime(model, 1), executor, LoopOptions{
		MaxTurns: 2, MaxTotalTokens: 1_000_000, Report: &report,
	}, false, nil); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != RunReportSchemaVersion {
		t.Fatalf("a finished request reported schema_version %d, want %d", report.SchemaVersion, RunReportSchemaVersion)
	}
	if report.RequestID == "" {
		t.Fatal("the report lost its request ID")
	}
}
