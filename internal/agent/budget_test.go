package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"Eylu/internal/config"
	contextledger "Eylu/internal/context"
	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/tool"
)

// The accumulated usage is reported separately from the last model call's usage,
// and reasoning tokens are not counted twice.
func TestRunReportsCumulativeUsage(t *testing.T) {
	usage := RunUsage{}
	calls := 0
	model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		calls++
		if calls < 3 {
			response := toolUseResponse("agent-"+string(rune('0'+calls)), protocol.ToolCall{ID: "call-" + string(rune('0'+calls)), Name: "echo", Arguments: json.RawMessage(`{}`)})
			// A provider that reports reasoning inside its output tokens.
			response.Usage = protocol.Usage{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 40, Exact: true}
			return response, nil
		}
		response := textResponse("final", "done")
		response.Usage = protocol.Usage{InputTokens: 10, OutputTokens: 5, Exact: true}
		return response, nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	response, err := conversation.Run(context.Background(), "usage", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 4, MaxTotalTokens: 1_000_000, Usage: &usage}, false, nil)
	if err != nil || response.Stop != protocol.StopCompleted {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
	if usage.ModelCalls != 3 || usage.RetryCalls != 0 || usage.SummaryCalls != 0 {
		t.Fatalf("usage = %#v", usage)
	}
	// Two tool rounds plus the final answer.
	if usage.InputTokens != 210 || usage.OutputTokens != 105 {
		t.Fatalf("usage = %#v", usage)
	}
	// Reasoning tokens are reported for observability but never added to the
	// budget, because the adapters already include them in output tokens.
	if usage.ReasoningTokens != 80 || usage.Total() != 315 {
		t.Fatalf("usage = %#v total = %d", usage, usage.Total())
	}
	if !usage.Exact || usage.Estimated {
		t.Fatalf("usage exactness = %#v", usage)
	}
	// The response keeps its own, much smaller usage.
	if response.Usage.InputTokens != 10 || response.Usage.OutputTokens != 5 {
		t.Fatalf("response usage = %#v", response.Usage)
	}
}

// A provider that omits usage makes the totals a lower bound instead of an exact
// figure.
func TestRunMarksEstimatedUsage(t *testing.T) {
	usage := RunUsage{}
	model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		response := textResponse("final", "done")
		response.Usage = protocol.Usage{}
		return response, nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	if _, err := NewConversation().Run(context.Background(), "estimate", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 2, MaxTotalTokens: 1_000_000, Usage: &usage}, false, nil); err != nil {
		t.Fatal(err)
	}
	if usage.Exact || !usage.Estimated || usage.Total() != 0 {
		t.Fatalf("usage = %#v", usage)
	}
}

// The budget boundary is inclusive: spending exactly the limit is allowed, one
// token more is not, and the pre-request check reserves the output.
func TestBudgetTrackerThresholdIsInclusive(t *testing.T) {
	tracker := newBudgetTracker(150)
	tracker.add(callMain, protocol.Usage{InputTokens: 100, OutputTokens: 50, Exact: true})
	if tracker.exceeded() {
		t.Fatal("spending exactly the limit was reported as exceeded")
	}
	tracker.add(callMain, protocol.Usage{InputTokens: 0, OutputTokens: 1, Exact: true})
	if !tracker.exceeded() {
		t.Fatal("spending one token over the limit was not reported as exceeded")
	}

	admission := newBudgetTracker(150)
	if !admission.admits(100, 50) {
		t.Fatal("a request that fits exactly was refused")
	}
	if admission.admits(100, 51) {
		t.Fatal("a request that overshoots the reserved output was admitted")
	}
	if admission.remaining() != 150 {
		t.Fatalf("remaining = %d", admission.remaining())
	}
	// An unset budget admits everything and reports no remaining tokens.
	unlimited := newBudgetTracker(0)
	if !unlimited.admits(1_000_000, 1_000_000) || unlimited.remaining() != 0 || unlimited.exceeded() {
		t.Fatal("an unset budget did not behave as unlimited")
	}
}

// A context-recovery retry is counted against the same request budget.
func TestRunCountsContextRecoveryRetries(t *testing.T) {
	usage := RunUsage{}
	attempts := 0
	model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		attempts++
		if attempts == 1 {
			return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrContextWindow, Message: "too long", ContextLimit: 32000}
		}
		return textResponse("final", "done"), nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	runtime := recoveryRuntime(t, model)
	if _, err := NewConversation().Run(context.Background(), "retry", runtime, executor, LoopOptions{MaxTurns: 1, MaxTotalTokens: 1_000_000, Usage: &usage}, false, nil); err != nil {
		t.Fatal(err)
	}
	if usage.RetryCalls != 1 || usage.ModelCalls != 1 {
		t.Fatalf("usage = %#v", usage)
	}
}

// Compaction is a model call inside the same request, so its usage is counted
// too and it is never mistaken for a main model call.
func TestRunCountsCompactionSummaryUsage(t *testing.T) {
	// The context layer reports the summary usage.
	callbackConversation := compactableConversation()
	recorded := make([]protocol.Usage, 0, 1)
	if _, _, err := callbackConversation.prepareRequestContext(context.Background(), compactionUsageRuntime(&compactionSummaryDriver{}), nil, func(usage protocol.Usage) {
		recorded = append(recorded, usage)
	}); err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 || recorded[0].InputTokens != 700 || recorded[0].OutputTokens != 90 {
		t.Fatalf("reported summary usage = %#v", recorded)
	}

	// The same usage reaches the request budget, and it is not counted as a main
	// model call.
	model := &compactionSummaryDriver{}
	conversation := compactableConversation()
	usage := RunUsage{}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	if _, err := conversation.Run(context.Background(), "compact", compactionUsageRuntime(model), executor, LoopOptions{MaxTurns: 1, MaxTotalTokens: 1_000_000, Usage: &usage}, false, nil); err != nil {
		t.Fatal(err)
	}
	// The summary and the main call each reported 700 input and 90 output tokens.
	if usage.SummaryCalls != 1 || usage.ModelCalls != 1 || usage.InputTokens != 1400 || usage.OutputTokens != 180 {
		t.Fatalf("usage = %#v", usage)
	}
}

// compactableConversation holds enough context across enough rounds for the
// automatic compactor to have an old candidate to omit.
func compactableConversation() *Conversation {
	conversation := NewConversation()
	for index := 0; index < 10; index++ {
		conversation.turns = append(conversation.turns,
			protocol.Turn{ID: fmt.Sprintf("user-%d", index), Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: fmt.Sprintf("goal-%d %s", index, strings.Repeat("u", 500))}}},
			protocol.Turn{ID: fmt.Sprintf("agent-%d", index), Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: strings.Repeat("a", 500)}}},
		)
	}
	return conversation
}

func compactionUsageRuntime(model driver.ModelDriver) Runtime {
	return Runtime{
		Provider: provider.Snapshot{Name: "work", Config: config.ProviderConfig{Adapter: model.Name(), BaseURL: "https://example.com/v1", Model: "gpt-5.6-sol", ContextWindow: 12_000}},
		Driver:   model, TokenEstimator: contextledger.ApproxEstimator{BytesPerToken: 1}, OutputReserveTokens: 200,
		ContextRecentRounds: 2, ContextCompactTrigger: 85, ContextCompactTarget: 60, MaxSummaryBytes: 512,
	}
}
