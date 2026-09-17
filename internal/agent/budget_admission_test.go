package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

// budgetSummaryDriver separates the two kinds of request a compaction produces:
// the summary call and the main call. Counting them apart is what lets a test say
// whether a paid summary was started.
type budgetSummaryDriver struct {
	mu        sync.Mutex
	summaries int
	main      int
}

func (d *budgetSummaryDriver) Name() string { return "budget-summary" }
func (d *budgetSummaryDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{}
}

func (d *budgetSummaryDriver) Generate(_ context.Context, request driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, turn := range request.Model.Turns {
		for _, part := range turn.Parts {
			if !strings.Contains(part.Text, "Create a compact handoff summary") {
				continue
			}
			d.summaries++
			return protocol.ModelResponse{
				Turn: protocol.Turn{ID: "summary", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: summaryFixture}}},
				Stop: protocol.StopCompleted, Usage: protocol.Usage{InputTokens: 700, OutputTokens: 90, Exact: true},
			}, nil
		}
	}
	d.main++
	return protocol.ModelResponse{
		Turn: protocol.Turn{ID: "main", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "done"}}},
		Stop: protocol.StopCompleted, Usage: protocol.Usage{InputTokens: 4, OutputTokens: 1, Exact: true},
	}, nil
}

func (d *budgetSummaryDriver) counts() (int, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.summaries, d.main
}

const summaryFixture = `<conversation_summary>
User goals:
- Continue implementation.
Constraints and decisions:
- Preserve task state.
Completed modifications:
- Changes complete.
Unfinished tasks:
- Verify.
Failed attempts:
- none
Validation results:
- tests passed
Key files:
- context_management.go
</conversation_summary>`

// A request that cannot afford the compaction summary does not start one.
//
// The summary is a paid model call, so starting it before the request budget has
// been consulted spends budget the request was told to respect. The deterministic
// compaction is chosen instead, which costs nothing.
func TestAnUnaffordableCompactionSummaryIsNotStarted(t *testing.T) {
	tests := []struct {
		name       string
		budget     int
		wantSummar int
	}{
		{name: "the request cannot afford it", budget: 1_000, wantSummar: 0},
		{name: "the request can afford it", budget: 1_000_000, wantSummar: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := &budgetSummaryDriver{}
			conversation := compactableConversation()
			executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
			report := RunReport{}
			usage := RunUsage{}
			_, err := conversation.Run(context.Background(), "compact", compactionUsageRuntime(model), executor, LoopOptions{
				MaxTurns: 1, MaxTotalTokens: test.budget, Report: &report, Usage: &usage,
			}, false, nil)
			summaries, _ := model.counts()
			if summaries != test.wantSummar {
				t.Fatalf("summary calls = %d, want %d (report %#v)", summaries, test.wantSummar, report)
			}
			if usage.SummaryCalls != test.wantSummar {
				t.Fatalf("summary calls counted = %d, want %d", usage.SummaryCalls, test.wantSummar)
			}
			compression := conversation.ContextReport().LastCompression
			if compression == nil {
				t.Fatal("the conversation was not compacted at all")
			}
			if test.wantSummar == 0 {
				if compression.Strategy != "deterministic_fallback" {
					t.Fatalf("strategy = %q, want the deterministic result", compression.Strategy)
				}
				if compression.Usage.InputTokens != 0 || compression.Usage.OutputTokens != 0 {
					t.Fatalf("a summary that was never started reported usage: %#v", compression.Usage)
				}
				if err == nil {
					t.Fatal("a request with a 1000 token budget was not stopped")
				}
			} else if compression.Strategy != "model" {
				t.Fatalf("strategy = %q, want the summary", compression.Strategy)
			}
		})
	}
}

// A model call is counted when it happens, whether or not the provider reported
// tokens, and the totals say they are a lower bound.
func TestACallIsCountedEvenWhenTheProviderReportsNoUsage(t *testing.T) {
	model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		response := textResponse("agent-1", "answer")
		response.Usage = protocol.Usage{}
		return response, nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	usage := RunUsage{}
	report := RunReport{}
	if _, err := NewConversation().Run(context.Background(), "silent", testRuntime(model, 1), executor, LoopOptions{
		MaxTurns: 2, MaxTotalTokens: 1_000_000, Usage: &usage, Report: &report,
	}, false, nil); err != nil {
		t.Fatalf("err = %v", err)
	}
	if usage.ModelCalls != 1 || report.ModelCalls != 1 {
		t.Fatalf("the call was not counted: usage=%#v report=%#v", usage, report)
	}
	if usage.Exact || !usage.Estimated || report.ExactUsage || !report.EstimatedUsage {
		t.Fatalf("a request with no reported usage claimed an exact total: usage=%#v report=%#v", usage, report)
	}
}
