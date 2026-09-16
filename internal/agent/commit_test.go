package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

// funcDriver returns scripted responses and records every request.
type funcDriver struct {
	name     string
	mu       sync.Mutex
	requests []driver.Request
	generate func(call int, request driver.Request) (protocol.ModelResponse, error)
}

func (d *funcDriver) Name() string { return d.name }
func (d *funcDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true}
}
func (d *funcDriver) Generate(ctx context.Context, request driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.mu.Lock()
	d.requests = append(d.requests, request)
	number := len(d.requests)
	d.mu.Unlock()
	return d.generate(number, request)
}

func (d *funcDriver) recorded() []driver.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]driver.Request(nil), d.requests...)
}

func toolUseResponse(turnID string, calls ...protocol.ToolCall) protocol.ModelResponse {
	parts := make([]protocol.Part, 0, len(calls))
	for index := range calls {
		call := calls[index]
		parts = append(parts, protocol.Part{Kind: protocol.PartToolCall, ToolCall: &call})
	}
	return protocol.ModelResponse{
		Turn: protocol.Turn{ID: turnID, Role: protocol.RoleAgent, Parts: parts},
		Stop: protocol.StopToolUse, Usage: protocol.Usage{InputTokens: 10, OutputTokens: 10},
	}
}

func textResponse(turnID, text string) protocol.ModelResponse {
	return protocol.ModelResponse{
		Turn: protocol.Turn{ID: turnID, Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: text}}},
		Stop: protocol.StopCompleted, Usage: protocol.Usage{InputTokens: 4, OutputTokens: 1},
	}
}

type countingEchoTool struct{ calls atomic.Int32 }

func (t *countingEchoTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: "echo", Description: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (t *countingEchoTool) Risk() policy.Risk  { return policy.RiskRead }
func (t *countingEchoTool) ParallelSafe() bool { return true }
func (t *countingEchoTool) Execute(context.Context, json.RawMessage) protocol.ToolResult {
	t.calls.Add(1)
	return protocol.ToolResult{Content: "echoed"}
}

// assertPairedCalls checks both directions of the call/result pairing: no call
// is left without a result and no result refers to a call the history lost.
func assertPairedCalls(t *testing.T, turns []protocol.Turn) {
	t.Helper()
	calls := make(map[string]bool)
	results := make(map[string]bool)
	for _, turn := range turns {
		for _, part := range turn.Parts {
			if part.ToolCall != nil && part.ToolCall.ID != "" {
				calls[part.ToolCall.ID] = true
			}
			if part.ToolResult != nil {
				results[part.ToolResult.CallID] = true
			}
		}
	}
	for id := range calls {
		if !results[id] {
			t.Fatalf("tool call %q has no terminal result in %#v", id, turns)
		}
	}
	for id := range results {
		if !calls[id] {
			t.Fatalf("tool result %q has no matching call in %#v", id, turns)
		}
	}
}

// A response that is committed and then aborted by the request budget must not
// leave an executable call behind, and the history must stay usable.
func TestRunBudgetExceededClosesCommittedToolCalls(t *testing.T) {
	item := &countingEchoTool{}
	model := &funcDriver{name: "loop", generate: func(number int, _ driver.Request) (protocol.ModelResponse, error) {
		if number == 1 {
			return toolUseResponse("agent-1", protocol.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)}), nil
		}
		return textResponse("agent-"+strconv.Itoa(number), "done"), nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	_, err := conversation.Run(context.Background(), "budget", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 3, MaxTotalTokens: 5}, false, nil)
	var protocolErr *protocol.Error
	if !errors.As(err, &protocolErr) || !strings.Contains(protocolErr.Message, "token budget") {
		t.Fatalf("err = %v", err)
	}
	if item.calls.Load() != 0 {
		t.Fatalf("tool executed %d times after the budget was exhausted", item.calls.Load())
	}
	turns := conversation.Transcript()
	assertPairedCalls(t, turns)
	if len(turns) != 3 || turns[2].Role != protocol.RoleTool {
		t.Fatalf("turns = %#v", turns)
	}
	result := turns[2].Parts[0].ToolResult
	if result == nil || result.State != protocol.CallNotExecuted || !strings.Contains(result.Content, "token budget") {
		t.Fatalf("closed result = %#v", result)
	}

	// The session keeps working, and the next request has no dangling call.
	response, err := conversation.Run(context.Background(), "again", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 3, MaxTotalTokens: 100}, false, nil)
	if err != nil || response.Stop != protocol.StopCompleted {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
	assertPairedCalls(t, conversation.Transcript())
	for _, request := range model.recorded() {
		assertPairedCalls(t, request.Model.Turns)
	}
}

// A malformed response must never reach the transcript.
func TestRunRejectsMalformedResponsesWithoutCommitting(t *testing.T) {
	tests := []struct {
		name     string
		response protocol.ModelResponse
		message  string
	}{
		{
			name:     "tool call without an ID",
			response: toolUseResponse("agent-1", protocol.ToolCall{Name: "echo", Arguments: json.RawMessage(`{}`)}),
			message:  "without an ID",
		},
		{
			name: "duplicate IDs in one response",
			response: toolUseResponse("agent-1",
				protocol.ToolCall{ID: "same", Name: "echo", Arguments: json.RawMessage(`{}`)},
				protocol.ToolCall{ID: "same", Name: "echo", Arguments: json.RawMessage(`{}`)}),
			message: "duplicate tool call ID",
		},
		{
			name:     "tool call without a name",
			response: toolUseResponse("agent-1", protocol.ToolCall{ID: "call-1", Arguments: json.RawMessage(`{}`)}),
			message:  "without a name",
		},
		{
			name:     "invalid JSON arguments",
			response: toolUseResponse("agent-1", protocol.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"broken"`)}),
			message:  "invalid JSON",
		},
		{
			name:     "tool use stop without calls",
			response: protocol.ModelResponse{Turn: protocol.Turn{ID: "agent-1", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "thinking"}}}, Stop: protocol.StopToolUse},
			message:  "without tool calls",
		},
		{
			name:     "completion that still requests tools",
			response: protocol.ModelResponse{Turn: protocol.Turn{ID: "agent-1", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartToolCall, ToolCall: &protocol.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)}}}}, Stop: protocol.StopCompleted},
			message:  "reported completion",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := &countingEchoTool{}
			model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
				return test.response, nil
			}}
			executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
			conversation := NewConversation()
			_, err := conversation.Run(context.Background(), "malformed", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 3}, false, nil)
			var protocolErr *protocol.Error
			if !errors.As(err, &protocolErr) || !strings.Contains(protocolErr.Message, test.message) {
				t.Fatalf("err = %v, want %q", err, test.message)
			}
			if item.calls.Load() != 0 {
				t.Fatalf("malformed response executed a tool %d times", item.calls.Load())
			}
			turns := conversation.Transcript()
			if len(turns) != 1 || turns[0].Role != protocol.RoleUser {
				t.Fatalf("transcript was polluted: %#v", turns)
			}
			if len(conversation.ExportState().DriverState) != 0 {
				t.Fatal("untrusted driver state was kept")
			}
		})
	}
}

// A tool call ID reused across responses is rejected before it is committed.
func TestRunRejectsRepeatedCallIDAcrossResponses(t *testing.T) {
	item := &countingEchoTool{}
	model := &funcDriver{name: "loop", generate: func(number int, _ driver.Request) (protocol.ModelResponse, error) {
		return toolUseResponse("agent-"+strconv.Itoa(number), protocol.ToolCall{ID: "reused", Name: "echo", Arguments: json.RawMessage(`{}`)}), nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	_, err := conversation.Run(context.Background(), "reuse", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 4}, false, nil)
	var protocolErr *protocol.Error
	if !errors.As(err, &protocolErr) || !strings.Contains(protocolErr.Message, "duplicate tool call ID") {
		t.Fatalf("err = %v", err)
	}
	if item.calls.Load() != 1 {
		t.Fatalf("calls = %d, want the first call only", item.calls.Load())
	}
	assertPairedCalls(t, conversation.Transcript())
}

// A registry refresh failure after the response was committed still closes the
// committed calls.
func TestRunRegistryRefreshFailureClosesCommittedCalls(t *testing.T) {
	item := &countingEchoTool{}
	var poison atomic.Bool
	model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		// The failure has to happen after the response is committed.
		poison.Store(true)
		return toolUseResponse("agent-1", protocol.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)}), nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
	runtime := testRuntime(model, 1)
	runtime.MCPState = func() MCPRuntimeState {
		if !poison.Load() {
			return MCPRuntimeState{}
		}
		// The same tool name as a registered base tool makes registration fail.
		return MCPRuntimeState{Tools: []tool.Tool{&countingEchoTool{}}}
	}
	conversation := NewConversation()
	_, err := conversation.Run(context.Background(), "refresh", runtime, executor, LoopOptions{MaxTurns: 3}, false, nil)
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("err = %v", err)
	}
	if item.calls.Load() != 0 {
		t.Fatalf("calls = %d", item.calls.Load())
	}
	turns := conversation.Transcript()
	assertPairedCalls(t, turns)
	if len(turns) != 3 {
		t.Fatalf("turns = %#v", turns)
	}
	result := turns[2].Parts[0].ToolResult
	if result == nil || result.State != protocol.CallNotExecuted {
		t.Fatalf("closed result = %#v", result)
	}
}

// A callback failure after part of a batch succeeded keeps the successful
// result and closes the calls that never started.
func TestRunResultCallbackFailureKeepsSuccessAndClosesRemaining(t *testing.T) {
	item := &countingEchoTool{}
	model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		return toolUseResponse("agent-1",
			protocol.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)},
			protocol.ToolCall{ID: "call-2", Name: "echo", Arguments: json.RawMessage(`{}`)}), nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}, MaxParallelTools: 1}
	conversation := NewConversation()
	sinkErr := errors.New("event sink closed")
	_, err := conversation.Run(context.Background(), "callback", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 3, MaxTotalTokens: 100}, false, func(event protocol.ModelEvent) error {
		if event.Kind == protocol.EventToolResult {
			return sinkErr
		}
		return nil
	})
	if !errors.Is(err, sinkErr) {
		t.Fatalf("err = %v", err)
	}
	if item.calls.Load() != 1 {
		t.Fatalf("calls = %d, want only the first call", item.calls.Load())
	}
	turns := conversation.Transcript()
	assertPairedCalls(t, turns)
	toolTurn := turns[2]
	if len(toolTurn.Parts) != 2 {
		t.Fatalf("tool turn = %#v", toolTurn)
	}
	first := toolTurn.Parts[0].ToolResult
	second := toolTurn.Parts[1].ToolResult
	if first == nil || first.IsError || first.State != protocol.CallSucceeded {
		t.Fatalf("first result was not preserved: %#v", first)
	}
	if second == nil || second.State != protocol.CallNotExecuted {
		t.Fatalf("second result = %#v", second)
	}
}

// A truncated response keeps its content but never executes a partial call.
func TestRunTruncatedResponseClosesPartialToolCalls(t *testing.T) {
	item := &countingEchoTool{}
	model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		response := toolUseResponse("agent-1", protocol.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"value":"partial"}`)})
		response.Stop = protocol.StopLength
		return response, nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	response, err := conversation.Run(context.Background(), "truncated", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 3}, false, nil)
	if err != nil || response.Stop != protocol.StopLength {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
	if item.calls.Load() != 0 {
		t.Fatalf("a truncated call was executed %d times", item.calls.Load())
	}
	turns := conversation.Transcript()
	assertPairedCalls(t, turns)
	if len(turns) != 3 || turns[2].Parts[0].ToolResult.State != protocol.CallNotExecuted {
		t.Fatalf("turns = %#v", turns)
	}
}

// Send and Adopt never run tools, so they close any call they record.
func TestSendAndAdoptCloseToolCalls(t *testing.T) {
	model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		return toolUseResponse("agent-1", protocol.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)}), nil
	}}
	conversation := NewConversation()
	if _, err := conversation.Send(context.Background(), "send", testRuntime(model, 1), false, nil); err != nil {
		t.Fatal(err)
	}
	turns := conversation.Transcript()
	assertPairedCalls(t, turns)
	if len(turns) != 3 || turns[2].Parts[0].ToolResult.State != protocol.CallNotExecuted {
		t.Fatalf("send turns = %#v", turns)
	}

	adopted := NewConversation()
	response := toolUseResponse("adopted-1", protocol.ToolCall{ID: "adopted-call", Name: "echo", Arguments: json.RawMessage(`{}`)})
	if err := adopted.Adopt("detached", testRuntime(model, 1), &response); err != nil {
		t.Fatal(err)
	}
	turns = adopted.Transcript()
	assertPairedCalls(t, turns)
	if len(turns) != 3 || turns[2].Parts[0].ToolResult.CallID != "adopted-call" || turns[2].Parts[0].ToolResult.State != protocol.CallNotExecuted {
		t.Fatalf("adopted turns = %#v", turns)
	}
}

// A historical call without a result is closed for the request only: the stored
// transcript keeps its original data and the diagnostic is reported separately.
func TestRestoredDanglingCallIsClosedForRequestOnly(t *testing.T) {
	model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		return textResponse("agent-2", "done"), nil
	}}
	state := NewConversation().ExportState()
	state.Turns = []protocol.Turn{
		{ID: "user-1", Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "first"}}},
		{ID: "agent-1", Role: protocol.RoleAgent, Parts: []protocol.Part{
			{Kind: protocol.PartText, Text: "calling"},
			{Kind: protocol.PartToolCall, ToolCall: &protocol.ToolCall{ID: "orphan", Name: "echo", Arguments: json.RawMessage(`{}`)}},
		}},
	}
	conversation, err := RestoreConversation(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conversation.Send(context.Background(), "second", testRuntime(model, 1), false, nil); err != nil {
		t.Fatal(err)
	}
	requests := model.recorded()
	if len(requests) != 1 {
		t.Fatalf("requests = %d", len(requests))
	}
	var recovered *protocol.ToolResult
	for _, turn := range requests[0].Model.Turns {
		for _, part := range turn.Parts {
			if part.ToolResult != nil && part.ToolResult.CallID == "orphan" {
				recovered = part.ToolResult
			}
		}
	}
	if recovered == nil {
		t.Fatalf("dangling call was sent to the model: %#v", requests[0].Model.Turns)
	}
	if recovered.State != protocol.CallOutcomeUnknown || !recovered.IsError {
		t.Fatalf("recovered result = %#v", recovered)
	}
	notes := conversation.RecoveryNotes()
	if len(notes) != 1 || notes[0] != "orphan" {
		t.Fatalf("recovery notes = %#v", notes)
	}
	// The stored transcript still holds the original, unrepaired turns: the
	// orphaned call keeps no result, and the recovery turn was never persisted.
	stored := conversation.Transcript()
	assertNoRecoveredTurns(t, stored)
	if len(stored) != 4 {
		t.Fatalf("stored transcript was rewritten: %#v", stored)
	}
	for _, turn := range stored {
		for _, part := range turn.Parts {
			if part.ToolResult != nil && part.ToolResult.CallID == "orphan" {
				t.Fatalf("the recovery result leaked into the stored transcript: %#v", stored)
			}
		}
	}
}

// Export, restore and run again keeps the session valid.
func TestExportRestoreAndRunKeepsPairing(t *testing.T) {
	item := &countingEchoTool{}
	model := &funcDriver{name: "loop", generate: func(number int, _ driver.Request) (protocol.ModelResponse, error) {
		if number == 1 {
			return toolUseResponse("agent-1", protocol.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)}), nil
		}
		return textResponse("agent-"+strconv.Itoa(number), "done"), nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	if _, err := conversation.Run(context.Background(), "first", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 3, MaxTotalTokens: 100}, false, nil); err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreConversation(conversation.ExportState())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restored.Run(context.Background(), "second", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 3, MaxTotalTokens: 100}, false, nil); err != nil {
		t.Fatal(err)
	}
	assertPairedCalls(t, restored.Transcript())
	for _, request := range model.recorded() {
		assertPairedCalls(t, request.Model.Turns)
	}
}

func assertNoRecoveredTurns(t *testing.T, turns []protocol.Turn) {
	t.Helper()
	for _, turn := range turns {
		if strings.HasPrefix(turn.ID, "recovered-") {
			t.Fatalf("recovery turn leaked into the stored transcript: %#v", turns)
		}
	}
}
