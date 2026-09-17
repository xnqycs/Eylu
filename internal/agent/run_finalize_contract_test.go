package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

// rejectingDriver streams one text delta and then answers with a response the
// request has to refuse, which is what a provider returning unusable tool
// arguments looks like.
type rejectingDriver struct {
	calls int
	// delta, when set, is streamed before the response is returned.
	delta string
}

func (*rejectingDriver) Name() string { return "rejecting" }
func (*rejectingDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true}
}
func (d *rejectingDriver) Generate(_ context.Context, _ driver.Request, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	d.calls++
	if d.delta != "" && emit != nil {
		if err := emit(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: d.delta}); err != nil {
			return protocol.ModelResponse{}, err
		}
	}
	return toolUseResponse("agent-1", protocol.ToolCall{
		ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{`),
	}), nil
}

// A response the request refuses must still enter the one exit path every other
// outcome uses: the report states why the request ended, the usage of the call
// that happened is published, and the request releases its pending set.
//
// Returning early here leaves the report empty and the run usage at zero, so the
// one outcome a caller most needs explained - a model that produced something the
// host would not accept - is the one with no explanation.
func TestRunFinalizesWhenTheModelResponseIsRejected(t *testing.T) {
	model := &rejectingDriver{}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	report := RunReport{}
	usage := RunUsage{}
	_, err := conversation.Run(context.Background(), "reject", testRuntime(model, 1), executor, LoopOptions{
		MaxTurns: 3, MaxTotalTokens: 1_000_000, Report: &report, Usage: &usage,
	}, false, nil)
	if err == nil {
		t.Fatal("the rejected response was accepted")
	}
	if !strings.Contains(err.Error(), "invalid JSON arguments") {
		t.Fatalf("err = %v", err)
	}
	// The refused response never became part of the history.
	for _, turn := range conversation.Transcript() {
		if turn.Role == protocol.RoleTool {
			t.Fatalf("a refused response was committed: %#v", turn)
		}
	}
	if report.StopReason != stopAborted || report.Error == "" {
		t.Fatalf("the report does not explain the rejection: %#v", report)
	}
	if report.ModelCalls != 1 || report.InputTokens != 10 || report.OutputTokens != 10 {
		t.Fatalf("the usage of the call that happened is missing: %#v", report)
	}
	if usage.ModelCalls != 1 || usage.InputTokens != report.InputTokens {
		t.Fatalf("the run usage was not published: %#v", usage)
	}
	if pending := conversation.PendingCalls(); len(pending) != 0 {
		t.Fatalf("the request did not release its pending set: %#v", pending)
	}
}

// A request that ends on a refused response must not leave its committed calls in
// the set the next request reads.
func TestRunReleasesThePendingSetWhenALaterResponseIsRejected(t *testing.T) {
	// The first round asks for a call and runs it; the second is refused.
	issues := 0
	model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		issues++
		if issues == 1 {
			return toolUseResponse("agent-1", protocol.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)}), nil
		}
		return toolUseResponse("agent-2", protocol.ToolCall{ID: "call-2", Name: "echo", Arguments: json.RawMessage(`{`)}), nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	report := RunReport{}
	if _, err := conversation.Run(context.Background(), "first", testRuntime(model, 1), executor, LoopOptions{
		MaxTurns: 3, MaxTotalTokens: 1_000_000, Report: &report,
	}, false, nil); err == nil {
		t.Fatal("the second response was accepted")
	}
	if pending := conversation.PendingCalls(); len(pending) != 0 {
		t.Fatalf("the finished request left %d call(s) in the set the next request reads: %#v", len(pending), pending)
	}

	// A second request on the same conversation describes only its own calls.
	second := RunReport{}
	if _, err := conversation.Run(context.Background(), "second", testRuntime(&readingDriver{}, 1), executor, LoopOptions{
		MaxTurns: 2, MaxTotalTokens: 1_000_000, Report: &second,
	}, false, nil); err != nil {
		t.Fatalf("second request: %v", err)
	}
	if second.PendingAtEnd != 0 || second.StopReason != "completed" {
		t.Fatalf("second report = %#v", second)
	}
}

// A model call that happened is accounted for even when the turn it produced
// cannot be persisted: whether the record could be written says nothing about
// whether the provider was paid.
func TestRunKeepsModelUsageWhenTheCommittedTurnCannotBePersisted(t *testing.T) {
	model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		response := textResponse("agent-1", "answer")
		response.Usage = protocol.Usage{InputTokens: 7, OutputTokens: 3, Exact: true}
		return response, nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	report := RunReport{}
	usage := RunUsage{}
	persistErr := errors.New("log unavailable")
	_, err := conversation.Run(context.Background(), "persist", testRuntime(model, 1), executor, LoopOptions{
		MaxTurns: 3, MaxTotalTokens: 1_000_000, Report: &report, Usage: &usage,
		OnTurnCommitted: func(protocol.Turn) error { return persistErr },
	}, false, nil)
	if !errors.Is(err, persistErr) {
		t.Fatalf("err = %v", err)
	}
	if report.StopReason != stopPersistenceFailed {
		t.Fatalf("stop reason = %q", report.StopReason)
	}
	if report.ModelCalls != 1 || usage.ModelCalls != 1 {
		t.Fatalf("the call that happened was not counted: report=%#v usage=%#v", report, usage)
	}
	if report.InputTokens != 7 || report.OutputTokens != 3 || !report.ExactUsage {
		t.Fatalf("the usage of the call that happened was lost: %#v", report)
	}
}

// A refused response ends the request through the same path as every other
// outcome, so the text the host has already received in a buffer is still
// delivered instead of being dropped with the response.
func TestRunDeliversBufferedTextBeforeARefusedResponseEnds(t *testing.T) {
	delivered := make([]string, 0, 2)
	model := &rejectingDriver{delta: "partial answer"}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	if _, err := conversation.Run(context.Background(), "reject", testRuntime(model, 1), executor, LoopOptions{
		MaxTurns: 3, MaxTotalTokens: 1_000_000,
	}, false, func(event protocol.ModelEvent) error {
		if event.Kind == protocol.EventTextDelta {
			delivered = append(delivered, event.Delta)
		}
		return nil
	}); err == nil {
		t.Fatal("the rejected response was accepted")
	}
	if strings.Join(delivered, "") != "partial answer" {
		t.Fatalf("the buffered text was lost with the response: %#v", delivered)
	}
}

// When the request already failed and the final flush fails too, the original
// failure stays the reason and the second one is preserved beside it rather than
// being dropped: a host that never received the last events has to be able to see
// that from the record.
func TestRunPreservesBothFailuresWhenTheFinalFlushAlsoFails(t *testing.T) {
	sentinel := errors.New("event sink closed")
	model := &rejectingDriver{delta: "partial answer"}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	report := RunReport{}
	_, err := conversation.Run(context.Background(), "reject", testRuntime(model, 1), executor, LoopOptions{
		MaxTurns: 3, MaxTotalTokens: 1_000_000, Report: &report,
	}, false, func(protocol.ModelEvent) error { return sentinel })
	if err == nil {
		t.Fatal("the rejected response was accepted")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("the final delivery failure was dropped: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid JSON arguments") {
		t.Fatalf("the original failure was overwritten: %v", err)
	}
	if report.StopReason != stopAborted {
		t.Fatalf("stop reason = %q, want the original failure's reason", report.StopReason)
	}
	joined := strings.Join(report.Warnings, "\n")
	if !strings.Contains(joined, "last host events") || !strings.Contains(joined, "event sink closed") {
		t.Fatalf("the report does not say the final events could not be delivered: %#v", report.Warnings)
	}
}
