package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

// The stop reason decides what a request does next. Only a genuine completion is
// reported as one.
func TestRunHandlesEveryStopReason(t *testing.T) {
	tests := []struct {
		name        string
		stop        protocol.StopKind
		withCall    bool
		wantErr     string
		wantCalls   int
		wantResults int
	}{
		{name: "completed", stop: protocol.StopCompleted},
		{name: "tool use", stop: protocol.StopToolUse, withCall: true, wantCalls: 1, wantResults: 1},
		{name: "length", stop: protocol.StopLength},
		{name: "length with a partial call", stop: protocol.StopLength, withCall: true, wantResults: 1},
		{name: "cancelled", stop: protocol.StopCancelled},
		{name: "error", stop: protocol.StopError, wantErr: "failed response"},
		{name: "unknown", stop: protocol.StopKind("half_finished"), wantErr: "unknown stop reason"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := &countingEchoTool{}
			first := true
			model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
				if !first {
					return textResponse("follow-up", "done"), nil
				}
				first = false
				response := textResponse("agent-1", "partial answer")
				response.Stop = test.stop
				if test.withCall {
					response.Turn.Parts = append(response.Turn.Parts, protocol.Part{Kind: protocol.PartToolCall, ToolCall: &protocol.ToolCall{
						ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`),
					}})
				}
				return response, nil
			}}
			executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
			conversation := NewConversation()
			response, err := conversation.Run(context.Background(), "stop", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 3, MaxTotalTokens: 1_000_000}, false, nil)
			switch {
			case test.wantErr != "":
				var protocolErr *protocol.Error
				if !errors.As(err, &protocolErr) || !strings.Contains(protocolErr.Message, test.wantErr) {
					t.Fatalf("err = %v, want %q", err, test.wantErr)
				}
			case err != nil:
				t.Fatalf("err = %v", err)
			}
			if item.calls.Load() != int32(test.wantCalls) {
				t.Fatalf("tool calls = %d, want %d", item.calls.Load(), test.wantCalls)
			}
			turns := conversation.Transcript()
			results := 0
			for _, turn := range turns {
				for _, part := range turn.Parts {
					if part.ToolResult == nil {
						continue
					}
					results++
					// Nothing was executed in these cases, so every result must say so.
					if test.wantCalls == 0 && part.ToolResult.State != protocol.CallNotExecuted {
						t.Fatalf("closed result = %#v", part.ToolResult)
					}
				}
			}
			if results != test.wantResults {
				t.Fatalf("results = %d, want %d", results, test.wantResults)
			}
			if test.stop == protocol.StopLength && err == nil && response.Stop != protocol.StopLength {
				t.Fatalf("response stop = %q", response.Stop)
			}
			if StopNote(test.stop) == "" && (test.stop == protocol.StopLength || test.stop == protocol.StopCancelled) {
				t.Fatalf("stop %q has no note", test.stop)
			}
			// Whatever happened, the stored history stays structurally valid.
			if _, restoreErr := RestoreConversation(conversation.ExportState()); restoreErr != nil {
				t.Fatalf("restore = %v", restoreErr)
			}
			assertPairedCalls(t, turns)
		})
	}
}

// A length cut can truncate the tool arguments in the middle. The partial answer
// is kept, the broken call is never executed, and the history stays restorable.
func TestRunKeepsPartialAnswerWithTruncatedToolArguments(t *testing.T) {
	item := &countingEchoTool{}
	model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		call := protocol.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"value":"unterminated`)}
		return protocol.ModelResponse{
			Turn: protocol.Turn{Role: protocol.RoleAgent, Parts: []protocol.Part{
				{Kind: protocol.PartText, Text: "partial answer"},
				{Kind: protocol.PartToolCall, ToolCall: &call},
			}},
			Stop: protocol.StopLength,
		}, nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	response, err := conversation.Run(context.Background(), "truncated", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 3, MaxTotalTokens: 1_000_000}, false, nil)
	if err != nil || response.Stop != protocol.StopLength {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
	if item.calls.Load() != 0 {
		t.Fatalf("a truncated call executed %d times", item.calls.Load())
	}
	turns := conversation.Transcript()
	agentTurn := turns[1]
	if len(agentTurn.Parts) != 2 || agentTurn.Parts[0].Text != "partial answer" {
		t.Fatalf("agent turn = %#v", agentTurn)
	}
	// The arguments were unusable, so they are recorded as an empty object rather
	// than an unparseable payload.
	if string(agentTurn.Parts[1].ToolCall.Arguments) != `{}` {
		t.Fatalf("arguments = %s", agentTurn.Parts[1].ToolCall.Arguments)
	}
	toolTurn := turns[2]
	if len(toolTurn.Parts) != 1 || toolTurn.Parts[0].ToolResult.State != protocol.CallNotExecuted || !strings.Contains(toolTurn.Parts[0].ToolResult.Content, "stopped the response") {
		t.Fatalf("tool turn = %#v", toolTurn)
	}
	if _, err := RestoreConversation(conversation.ExportState()); err != nil {
		t.Fatalf("restore = %v", err)
	}
}

// A call that cannot be paired at all is dropped, while the answer is kept.
func TestRunDropsUnpairablePartialCallFromTruncatedAnswer(t *testing.T) {
	model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		first := protocol.ToolCall{ID: "keep", Name: "echo", Arguments: json.RawMessage(`{}`)}
		second := protocol.ToolCall{ID: "", Name: "echo", Arguments: json.RawMessage(`{}`)}
		return protocol.ModelResponse{
			Turn: protocol.Turn{Role: protocol.RoleAgent, Parts: []protocol.Part{
				{Kind: protocol.PartText, Text: "partial"},
				{Kind: protocol.PartToolCall, ToolCall: &first},
				{Kind: protocol.PartToolCall, ToolCall: &second},
			}},
			Stop: protocol.StopLength,
		}, nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	if _, err := conversation.Run(context.Background(), "truncated", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 3, MaxTotalTokens: 1_000_000}, false, nil); err != nil {
		t.Fatal(err)
	}
	turns := conversation.Transcript()
	if len(turns[1].Parts) != 2 || turns[1].Parts[0].Text != "partial" {
		t.Fatalf("agent turn = %#v", turns[1])
	}
	if _, err := RestoreConversation(conversation.ExportState()); err != nil {
		t.Fatalf("the dropped call left the history unrestorable: %v", err)
	}
}

// Reaching the iteration limit keeps a usable and restorable history.
func TestRunIterationLimitKeepsRecoverableHistory(t *testing.T) {
	item := &countingEchoTool{}
	issued := 0
	model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		issued++
		call := protocol.ToolCall{ID: "call-" + strconv.Itoa(issued), Name: "echo", Arguments: json.RawMessage(`{}`)}
		return toolUseResponse("agent-"+strconv.Itoa(issued), call), nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	_, err := conversation.Run(context.Background(), "loop", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 2, MaxTotalTokens: 1_000_000}, false, nil)
	var protocolErr *protocol.Error
	if !errors.As(err, &protocolErr) || !strings.Contains(protocolErr.Message, "iteration limit") {
		t.Fatalf("err = %v", err)
	}
	turns := conversation.Transcript()
	assertPairedCalls(t, turns)
	restored, err := RestoreConversation(conversation.ExportState())
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Transcript()) != len(turns) {
		t.Fatalf("restored turns = %d, want %d", len(restored.Transcript()), len(turns))
	}
}
