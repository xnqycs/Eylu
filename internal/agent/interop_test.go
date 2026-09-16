package agent

import (
	"context"
	"encoding/json"
	"testing"

	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/testfault"
	"Eylu/internal/tool"
)

// A provider that needs the interoperability relaxation still runs a normal
// request, and the relaxation is recorded on the run report: a relaxed request
// must never be indistinguishable from a conforming one.
func TestRelaxedStopIsConfiguredReachesTheDriverAndIsRecorded(t *testing.T) {
	item := &countingEchoTool{}
	seen := false
	calls := 0
	model := &testfault.DriverFaults{
		Delegate: &funcDriver{name: "interop", generate: func(_ int, request driver.Request) (protocol.ModelResponse, error) {
			calls++
			if calls > 1 {
				return textResponse("agent-2", "done"), nil
			}
			seen = request.AcceptToolCallsWithStop
			return toolUseResponse("agent-1", protocol.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)}), nil
		}},
		Rewrite: func(response *protocol.ModelResponse) {
			for _, part := range response.Turn.Parts {
				if part.Kind != protocol.PartToolCall {
					continue
				}
				// Stand in for a gateway that reports a normal stop beside its
				// calls: the driver applied the relaxation and named it.
				response.Stop = protocol.StopToolUse
				response.Interop = append(response.Interop, driver.InteropNoteToolCallsWithStop)
				return
			}
		},
	}
	runtime := testRuntime(model, 1)
	runtime.Provider.Config.AcceptToolCallsWithStop = true
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
	report := RunReport{}
	conversation := NewConversation()
	if _, err := conversation.Run(context.Background(), "relaxed", runtime, executor,
		LoopOptions{MaxTurns: 2, MaxTotalTokens: 1_000_000, Report: &report}, false, nil); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !seen {
		t.Fatal("the configured relaxation never reached the driver")
	}
	if item.calls.Load() != 1 {
		t.Fatalf("the relaxed tool call ran %d times, want once", item.calls.Load())
	}
	if len(report.Interop) != 1 || report.Interop[0] != driver.InteropNoteToolCallsWithStop {
		t.Fatalf("report interop = %#v", report.Interop)
	}
	if report.StopReason != string(protocol.StopCompleted) {
		t.Fatalf("stop reason = %q", report.StopReason)
	}
}

// A conforming request records nothing, so the presence of a note is meaningful.
func TestConformingStopRecordsNoRelaxation(t *testing.T) {
	item := &countingEchoTool{}
	calls := 0
	model := &funcDriver{name: "plain", generate: func(_ int, request driver.Request) (protocol.ModelResponse, error) {
		calls++
		if request.AcceptToolCallsWithStop {
			t.Fatal("the relaxation was enabled without being configured")
		}
		if calls > 1 {
			return textResponse("agent-2", "done"), nil
		}
		return toolUseResponse("agent-1", protocol.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)}), nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
	report := RunReport{}
	conversation := NewConversation()
	if _, err := conversation.Run(context.Background(), "plain", testRuntime(model, 1), executor,
		LoopOptions{MaxTurns: 2, MaxTotalTokens: 1_000_000, Report: &report}, false, nil); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(report.Interop) != 0 {
		t.Fatalf("report interop = %#v, want none", report.Interop)
	}
}
