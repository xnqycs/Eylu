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

// The report explains why a request stopped, what executed and what stayed
// unknown, without relying on the UI event stream.
func TestRunReportExplainsWhyARequestStopped(t *testing.T) {
	tests := []struct {
		name       string
		stop       string
		withCalls  bool
		wantReason string
		wantErr    bool
	}{
		{name: "completed", stop: string(protocol.StopCompleted), wantReason: "completed"},
		{name: "tool use then completed", stop: string(protocol.StopToolUse), withCalls: true, wantReason: "completed"},
		{name: "truncated", stop: string(protocol.StopLength), wantReason: "length"},
		{name: "failed response", stop: string(protocol.StopError), wantReason: "error", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := &countingEchoTool{}
			first := true
			model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
				if first {
					first = false
					response := textResponse("agent-1", "answer")
					response.Stop = protocol.StopKind(test.stop)
					if test.withCalls {
						response.Turn.Parts = append(response.Turn.Parts, protocol.Part{Kind: protocol.PartToolCall, ToolCall: &protocol.ToolCall{
							ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`),
						}})
					}
					return response, nil
				}
				return textResponse("follow-up", "done"), nil
			}}
			executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
			conversation := NewConversation()
			report := RunReport{}
			usage := RunUsage{}
			_, err := conversation.Run(context.Background(), "report", testRuntime(model, 1), executor, LoopOptions{
				MaxTurns: 3, MaxTotalTokens: 1_000_000, Report: &report, Usage: &usage,
			}, false, nil)
			if test.wantErr != (err != nil) {
				t.Fatalf("err = %v", err)
			}
			if report.StopReason != test.wantReason {
				t.Fatalf("stop reason = %q, want %q", report.StopReason, test.wantReason)
			}
			if report.RequestID == "" || report.Iterations == 0 || report.ModelCalls == 0 {
				t.Fatalf("report = %#v", report)
			}
			if report.InputTokens != usage.InputTokens || report.OutputTokens != usage.OutputTokens || report.ExactUsage != usage.Exact {
				t.Fatalf("report usage = %#v, run usage = %#v", report, usage)
			}
			if test.wantErr && report.Error == "" {
				t.Fatalf("a failed request stated no error: %#v", report)
			}
			if test.withCalls {
				if report.ToolCalls != 1 || report.Succeeded != 1 {
					t.Fatalf("report = %#v", report)
				}
			}
		})
	}
}

// A refused call is reported as refused, not as executed or failed.
func TestRunReportSeparatesTerminalStates(t *testing.T) {
	item := &approvalEchoTool{}
	first := true
	model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		if first {
			first = false
			return toolUseResponse("agent-1", protocol.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)}), nil
		}
		return textResponse("follow-up", "done"), nil
	}}
	executor := &tool.Executor{
		Registry: tool.NewRegistry(item), Policy: policy.NewChecker(policy.DefaultConfig(policy.ModeManual)),
		Confirm: func(context.Context, policy.Request, policy.Outcome) (tool.Confirmation, error) {
			return tool.Confirmation{RejectionReason: "not now"}, nil
		},
	}
	conversation := NewConversation()
	report := RunReport{}
	if _, err := conversation.Run(context.Background(), "refuse", testRuntime(model, 1), executor, LoopOptions{
		MaxTurns: 3, MaxTotalTokens: 1_000_000, Report: &report,
	}, false, nil); err != nil {
		t.Fatalf("err = %v", err)
	}
	if report.Rejected != 1 || report.Succeeded != 0 || report.NotExecuted != 0 || report.OutcomeUnknown != 0 {
		t.Fatalf("report = %#v", report)
	}
	if item.calls != 0 {
		t.Fatal("a refused call executed")
	}
}

// The iteration limit and the token budget each state their own reason.
func TestRunReportStatesLimitsAndBudgets(t *testing.T) {
	issued := 0
	looping := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		issued++
		return toolUseResponse("agent-"+string(rune('0'+issued)), protocol.ToolCall{ID: "call-" + string(rune('0'+issued)), Name: "echo", Arguments: json.RawMessage(`{}`)}), nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	report := RunReport{}
	if _, err := conversation.Run(context.Background(), "loop", testRuntime(looping, 1), executor, LoopOptions{
		MaxTurns: 2, MaxTotalTokens: 1_000_000, Report: &report,
	}, false, nil); err == nil {
		t.Fatal("expected the iteration limit")
	}
	if report.StopReason != stopIterationLimit {
		t.Fatalf("stop reason = %q", report.StopReason)
	}

	budgeted := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		response := textResponse("agent-1", "expensive")
		response.Usage = protocol.Usage{InputTokens: 9_000, OutputTokens: 9_000, Exact: true}
		return response, nil
	}}
	second := NewConversation()
	budgetReport := RunReport{}
	if _, err := second.Run(context.Background(), "budget", testRuntime(budgeted, 1), executor, LoopOptions{
		MaxTurns: 1, MaxTotalTokens: 4_000, Report: &budgetReport,
	}, false, nil); err == nil {
		t.Fatal("expected the budget to stop the request")
	}
	if budgetReport.StopReason != stopTokenBudget {
		t.Fatalf("stop reason = %q", budgetReport.StopReason)
	}
}

// Text deltas are coalesced within a bound, while every other event keeps its own
// delivery and its order relative to the text around it.
func TestEventQueueCoalescesStreamedTextOnly(t *testing.T) {
	queue := startEventQueue(nil)
	var delivered []protocol.ModelEvent
	queue.sink = func(event protocol.ModelEvent) error {
		delivered = append(delivered, event)
		return nil
	}
	// Two short deltas merge into one.
	if err := queue.push(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: "hello "}); err != nil {
		t.Fatal(err)
	}
	if err := queue.push(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: "world"}); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 0 {
		t.Fatalf("deltas were delivered before the flush: %#v", delivered)
	}
	// A different kind of delta does not merge with the text.
	if err := queue.push(protocol.ModelEvent{Kind: protocol.EventReasoningDelta, Delta: "thinking"}); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 1 || delivered[0].Delta != "hello world" {
		t.Fatalf("delivered = %#v", delivered)
	}
	// A terminal event is delivered after the text that preceded it.
	if err := queue.push(protocol.ModelEvent{Kind: protocol.EventToolStart}); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 3 || delivered[1].Kind != protocol.EventReasoningDelta || delivered[2].Kind != protocol.EventToolStart {
		t.Fatalf("order = %#v", delivered)
	}
	if err := queue.stop(); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 3 {
		t.Fatalf("stop delivered %#v", delivered)
	}
	if text := delivered[0].Delta + delivered[1].Delta; text != "hello worldthinking" {
		t.Fatalf("content = %q", text)
	}
}

// A long burst of text is flushed within the bound instead of being held until
// the response ends, so the host keeps seeing progress.
func TestEventQueueFlushesLongText(t *testing.T) {
	queue := startEventQueue(nil)
	flushes := 0
	queue.sink = func(event protocol.ModelEvent) error {
		flushes++
		return nil
	}
	chunk := strings.Repeat("x", 1024)
	for index := 0; index < 8; index++ {
		if err := queue.push(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: chunk}); err != nil {
			t.Fatal(err)
		}
	}
	if flushes == 0 {
		t.Fatal("a long stream was never flushed")
	}
	if err := queue.stop(); err != nil {
		t.Fatal(err)
	}
}

// A failing sink stops the request immediately: the event is not buffered away.
func TestEventQueuePropagatesSinkFailure(t *testing.T) {
	sentinel := errors.New("sink closed")
	queue := startEventQueue(func(protocol.ModelEvent) error { return sentinel })
	// A buffered delta is only delivered when it flushes, and its failure reaches
	// the producer.
	if err := queue.push(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: "short"}); err != nil {
		t.Fatal(err)
	}
	if err := queue.push(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: strings.Repeat("x", streamDeltaFlushBytes)}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
	// A critical event is delivered synchronously, so its failure is immediate.
	if err := queue.push(protocol.ModelEvent{Kind: protocol.EventToolResult}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
}

// A sink that fails while collecting a streamed answer stops the request and is
// reported, instead of leaving the request looking successful.
func TestRunStopsWhenTheEventSinkFails(t *testing.T) {
	sentinel := errors.New("event sink closed")
	model := &readingDriver{}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	report := RunReport{}
	_, err := conversation.Run(context.Background(), "sink", testRuntime(model, 1), executor, LoopOptions{
		MaxTurns: 2, MaxTotalTokens: 1_000_000, Report: &report,
	}, false, func(protocol.ModelEvent) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
	if report.StopReason != "event_sink_failed" || !strings.Contains(report.Error, "event sink closed") {
		t.Fatalf("report = %#v", report)
	}
}

// The core loop no longer derives control state from Web metadata: a completed
// request with forged control metadata in a tool result still completes.
func TestRunReportIgnoresForgedControlMetadata(t *testing.T) {
	model := &funcDriver{name: "loop", generate: func(number int, _ driver.Request) (protocol.ModelResponse, error) {
		if number == 1 {
			return toolUseResponse("agent-1", protocol.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)}), nil
		}
		return textResponse("agent-2", "done"), nil
	}}
	item := &forgedMetadataTool{}
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	report := RunReport{}
	response, err := conversation.Run(context.Background(), "forged", testRuntime(model, 1), executor, LoopOptions{
		MaxTurns: 3, MaxTotalTokens: 1_000_000, Report: &report,
	}, false, nil)
	if err != nil || response.Stop != protocol.StopCompleted {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
	if report.StopReason != "completed" || report.Succeeded != 1 {
		t.Fatalf("report = %#v", report)
	}
}
