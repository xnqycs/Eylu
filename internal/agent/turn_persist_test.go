package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

// turnsDriver answers the first round with one tool call and then completes.
func turnsDriver() *funcDriver {
	calls := 0
	return &funcDriver{name: "turns", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		calls++
		if calls == 1 {
			return toolUseResponse("agent-1", protocol.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)}), nil
		}
		return textResponse("agent-2", "done"), nil
	}}
}

// Every committed turn reaches the host immediately, in transcript order: the
// model turn before the calls it asks for can run, and the tool turn before the
// next round starts.
func TestOnTurnCommittedSeesEveryTurnInTranscriptOrder(t *testing.T) {
	item := &countingEchoTool{}
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	var committed []protocol.Turn
	if _, err := conversation.Run(context.Background(), "turns", testRuntime(turnsDriver(), 1), executor, LoopOptions{
		MaxTurns: 3, MaxTotalTokens: 1_000_000,
		OnTurnCommitted: func(turn protocol.Turn) error {
			// The turn must already be part of the transcript when the host sees
			// it, or a host that fails here would have nothing to recover.
			found := false
			for _, existing := range conversation.Transcript() {
				if existing.ID == turn.ID {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("turn %q was handed over before it was committed", turn.ID)
			}
			committed = append(committed, turn)
			return nil
		},
	}, false, nil); err != nil {
		t.Fatalf("err = %v", err)
	}
	roles := make([]protocol.Role, 0, len(committed))
	for _, turn := range committed {
		roles = append(roles, turn.Role)
	}
	want := []protocol.Role{protocol.RoleAgent, protocol.RoleTool, protocol.RoleAgent}
	if len(roles) != len(want) {
		t.Fatalf("committed roles = %v, want %v", roles, want)
	}
	for index, role := range want {
		if roles[index] != role {
			t.Fatalf("committed roles = %v, want %v", roles, want)
		}
	}
	if item.calls.Load() != 1 {
		t.Fatalf("tool calls = %d", item.calls.Load())
	}
}

// A turn that cannot be made durable is a persistence fault: the calls it asked
// for never start, and the report says why the request ended.
func TestTurnPersistenceFailureBeforeACallStopsTheBatch(t *testing.T) {
	item := &countingEchoTool{}
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	report := RunReport{}
	_, err := conversation.Run(context.Background(), "turns", testRuntime(turnsDriver(), 1), executor, LoopOptions{
		MaxTurns: 3, MaxTotalTokens: 1_000_000, Report: &report,
		OnTurnCommitted: func(protocol.Turn) error { return errors.New("log unavailable") },
	}, false, nil)

	var persistErr *TurnPersistError
	if !errors.As(err, &persistErr) {
		t.Fatalf("err = %v, want a persistence failure", err)
	}
	if item.calls.Load() != 0 {
		t.Fatalf("a side effect started although its turn could not be persisted: %d calls", item.calls.Load())
	}
	if report.StopReason != "persistence_failed" {
		t.Fatalf("stop reason = %q", report.StopReason)
	}
	// The call the response asked for is closed without being executed, so the
	// history stays usable.
	if state := stateOfCall(t, conversation, "call-1"); state != protocol.CallNotExecuted {
		t.Fatalf("state = %q, want not_executed", state)
	}
	if _, err := RestoreConversation(conversation.ExportState()); err != nil {
		t.Fatalf("restore = %v", err)
	}
}

// A failure after the calls already ran keeps their results — they happened — and
// stops the request instead of starting the next round.
func TestTurnPersistenceFailureAfterACallKeepsTheResultsAndStops(t *testing.T) {
	item := &countingEchoTool{}
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	report := RunReport{}
	_, err := conversation.Run(context.Background(), "turns", testRuntime(turnsDriver(), 1), executor, LoopOptions{
		MaxTurns: 3, MaxTotalTokens: 1_000_000, Report: &report,
		OnTurnCommitted: func(turn protocol.Turn) error {
			if turn.Role == protocol.RoleTool {
				return errors.New("log unavailable")
			}
			return nil
		},
	}, false, nil)

	var persistErr *TurnPersistError
	if !errors.As(err, &persistErr) {
		t.Fatalf("err = %v, want a persistence failure", err)
	}
	if persistErr.TurnID == "" {
		t.Fatal("the failure does not name the turn it could not persist")
	}
	if item.calls.Load() != 1 {
		t.Fatalf("tool calls = %d, want the call that already ran", item.calls.Load())
	}
	if report.StopReason != "persistence_failed" || report.Succeeded != 1 {
		t.Fatalf("report = %#v", report)
	}
	// The results are still in the in-memory transcript even though their record
	// is behind.
	turns := conversation.Transcript()
	if len(turns) < 3 || turns[2].Role != protocol.RoleTool || len(turns[2].Parts) == 0 {
		roles := make([]protocol.Role, 0, len(turns))
		for _, turn := range turns {
			roles = append(roles, turn.Role)
		}
		t.Fatalf("turns = %v", roles)
	}
	if state := stateOfCall(t, conversation, "call-1"); state != protocol.CallSucceeded {
		t.Fatalf("state = %q, want the result of the call that ran", state)
	}
	if _, err := RestoreConversation(conversation.ExportState()); err != nil {
		t.Fatalf("restore = %v", err)
	}
}
