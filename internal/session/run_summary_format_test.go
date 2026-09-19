package session

import (
	"path/filepath"
	"testing"
	"time"

	"Eylu/internal/docscheck"
)

// TestRunSummaryKeepsItsCommittedFieldContract is the machine-checkable half of
// the durable run summary's promise.
//
// The session document carries its own schema version, but a summary is also read
// on its own - from a `last_run` field or an exported event - and a reader there
// has no way to ask which build wrote it. That is why it names its own shape, and
// this test is what keeps the name honest.
func TestRunSummaryKeepsItsCommittedFieldContract(t *testing.T) {
	docscheck.CheckFieldContract(t, filepath.Join("testdata", "run_summary_format.txt"), RunSummary{
		SchemaVersion: RunSummarySchemaVersion,
		RequestID:     "request", Iterations: 2, StopReason: "completed", Error: "failure",
		ModelCalls: 3, ToolCalls: 4, Succeeded: 1, Failed: 1, Rejected: 1, Cancelled: 1, NotExecuted: 1, OutcomeUnknown: 1,
		InputTokens: 10, OutputTokens: 20, ExactUsage: true,
		ReportedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
}
