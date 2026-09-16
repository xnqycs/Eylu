package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

// pendingSnapshot is the part of the pending view a stage assertion needs.
type pendingSnapshot struct {
	calls    []PendingCall
	prepared int
	open     int
}

func snapshotPending(conversation *Conversation) pendingSnapshot {
	calls := conversation.PendingCalls()
	snapshot := pendingSnapshot{calls: calls}
	for _, call := range calls {
		if call.Prepared {
			snapshot.prepared++
		}
		if call.State == "" {
			snapshot.open++
		}
	}
	return snapshot
}

// The pending set describes every stage of a call's life, and it is emptied when
// the request ends: a request that leaves a call open is the defect the run report
// publishes.
//
// The stages are observed from the hooks, which are the barriers the loop itself
// uses, so nothing here depends on timing.
func TestPendingSetDescribesEveryStageOfACall(t *testing.T) {
	item := newGatedTool("blocked", policy.RiskRead)
	model := &batchDriver{calls: []protocol.ToolCall{
		{ID: "one", Name: "blocked", Arguments: json.RawMessage(`{}`)},
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	report := RunReport{}

	type stage struct {
		name     string
		total    int
		prepared int
		open     int
		state    protocol.CallState
	}
	stages := make([]stage, 0, 3)
	observe := func(name string) {
		snapshot := snapshotPending(conversation)
		entry := stage{name: name, total: len(snapshot.calls), prepared: snapshot.prepared, open: snapshot.open}
		if len(snapshot.calls) == 1 {
			entry.state = snapshot.calls[0].State
		}
		stages = append(stages, entry)
	}

	var preparedErr error
	done := make(chan error, 1)
	go func() {
		_, err := conversation.Run(context.Background(), "pending", testRuntime(model, 1), executor, LoopOptions{
			MaxTurns: 2, MaxTotalTokens: 1_000_000, Report: &report,
			OnTurnCommitted: func(turn protocol.Turn) error {
				switch turn.Role {
				case protocol.RoleAgent:
					// The turn is committed; the batch has not started.
					observe("committed")
				case protocol.RoleTool:
					// The results are committed; the call has a terminal state.
					observe("terminal")
				}
				return nil
			},
			OnToolPrepared: func(protocol.ToolCall) error {
				// The executor accepted the call for execution.
				observe("prepared")
				return preparedErr
			},
		}, false, nil)
		done <- err
	}()
	close(item.release)
	if err := <-done; err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(stages) != 4 {
		t.Fatalf("observed stages = %#v", stages)
	}
	committed, prepared, terminal, later := stages[0], stages[1], stages[2], stages[3]
	if committed.name != "committed" || committed.total != 1 || committed.prepared != 0 || committed.open != 1 || committed.state != "" {
		t.Fatalf("committed stage = %#v, want one open, unprepared call", committed)
	}
	if prepared.name != "prepared" || prepared.total != 1 || prepared.prepared != 1 || prepared.open != 1 {
		t.Fatalf("prepared stage = %#v, want the call marked prepared and still open", prepared)
	}
	if terminal.name != "terminal" || terminal.total != 1 || terminal.open != 0 || terminal.state != protocol.CallSucceeded {
		t.Fatalf("terminal stage = %#v, want a closed, succeeded call", terminal)
	}
	// The second round commits another turn, and the earlier call stays closed:
	// a later turn never reopens what already reached a terminal state.
	if later.open != 0 || later.state != protocol.CallSucceeded {
		t.Fatalf("later stage = %#v, want the closed call left alone", later)
	}
	// The set belongs to the request, so the request ends with it empty.
	if left := conversation.PendingCalls(); len(left) != 0 {
		t.Fatalf("the pending set outlived its request: %#v", left)
	}
	if report.PendingAtEnd != 0 {
		t.Fatalf("report pending at end = %d, want 0", report.PendingAtEnd)
	}
}

// A request that ends with a call still open is a defect, and the run report says
// so with the call named instead of leaving it to be discovered later.
func TestAnOpenCallAtTheEndIsReportedAsADefect(t *testing.T) {
	conversation := NewConversation()
	conversation.mu.Lock()
	conversation.trackCommittedCalls("request-1", 1, protocol.Turn{
		ID: "agent-1", Role: protocol.RoleAgent,
		Parts: []protocol.Part{{Kind: protocol.PartToolCall, ToolCall: &protocol.ToolCall{ID: "stuck", Name: "echo", Arguments: json.RawMessage(`{}`)}}},
	})
	conversation.mu.Unlock()

	report := RunReport{}
	finalizer := &runFinalizer{conversation: conversation, budget: newBudgetTracker(0), report: &report}
	finalizer.publish("completed", nil)

	if report.PendingAtEnd != 1 {
		t.Fatalf("pending at end = %d, want the open call counted", report.PendingAtEnd)
	}
	if len(report.Warnings) != 1 || !containsSubstring(report.Warnings[0], "stuck") {
		t.Fatalf("warnings = %#v, want the open call named", report.Warnings)
	}
	if left := conversation.PendingCalls(); len(left) != 0 {
		t.Fatalf("the set was not cleared: %#v", left)
	}
}

// The pending set and the recovery path must reach the same conclusion about a
// call: one implementation closes a live request, the other closes a restored
// transcript, and a call that ran is terminal in both.
func TestPendingSetAgreesWithTheRecoveryPath(t *testing.T) {
	item := &countingEchoTool{}
	model := &batchDriver{calls: []protocol.ToolCall{
		{ID: "one", Name: "echo", Arguments: json.RawMessage(`{}`)},
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	report := RunReport{}
	var closedBySet protocol.CallState
	if _, err := conversation.Run(context.Background(), "agree", testRuntime(model, 1), executor, LoopOptions{
		MaxTurns: 2, MaxTotalTokens: 1_000_000, Report: &report,
		OnTurnCommitted: func(turn protocol.Turn) error {
			if turn.Role != protocol.RoleTool {
				return nil
			}
			// Capture what the set says before the request ends and clears it.
			for _, entry := range conversation.PendingCalls() {
				if entry.CallID == "one" {
					closedBySet = entry.State
				}
			}
			return nil
		},
	}, false, nil); err != nil {
		t.Fatalf("err = %v", err)
	}
	if closedBySet != protocol.CallSucceeded {
		t.Fatalf("the set reported %q", closedBySet)
	}
	// The recovery path rebuilds the same conclusion from the transcript alone.
	restored, err := RestoreConversation(conversation.ExportState())
	if err != nil {
		t.Fatalf("restore = %v", err)
	}
	recovered := ""
	for _, turn := range restored.Transcript() {
		for _, part := range turn.Parts {
			if part.ToolResult != nil && part.ToolResult.CallID == "one" {
				recovered = string(part.ToolResult.State)
			}
		}
	}
	if recovered != string(closedBySet) {
		t.Fatalf("the recovery path says %q and the pending set says %q", recovered, closedBySet)
	}
	if report.PendingAtEnd != 0 {
		t.Fatalf("pending at end = %d", report.PendingAtEnd)
	}
}

// A prepared call whose request is stopped before the batch runs is closed by the
// set, so the set is empty at the end even on that path.
func TestACallStoppedBeforeExecutionIsClosedByTheSet(t *testing.T) {
	item := &countingEchoTool{}
	model := &batchDriver{calls: []protocol.ToolCall{
		{ID: "one", Name: "echo", Arguments: json.RawMessage(`{}`)},
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	report := RunReport{}
	if _, err := conversation.Run(context.Background(), "stop", testRuntime(model, 1), executor, LoopOptions{
		MaxTurns: 2, MaxTotalTokens: 1_000_000, Report: &report,
		OnToolPrepared: func(protocol.ToolCall) error {
			return errors.New("the host event could not be delivered")
		},
	}, false, nil); err == nil {
		t.Fatal("the hook failure was not reported")
	}
	if item.calls.Load() != 0 {
		t.Fatalf("the tool ran %d times after the batch was aborted", item.calls.Load())
	}
	if report.PendingAtEnd != 0 {
		t.Fatalf("pending at end = %d, want the set closed", report.PendingAtEnd)
	}
}

// A request whose calls are closed without ever executing still leaves the set
// empty, and it is the closing path that has to do it: no batch result exists to
// carry the state, so a set that only learned from results would stay open and the
// report would call a correct request defective.
func TestCallsClosedWithoutExecutionLeaveTheSetEmpty(t *testing.T) {
	item := &countingEchoTool{}
	model := &batchDriver{calls: []protocol.ToolCall{
		{ID: "one", Name: "echo", Arguments: json.RawMessage(`{}`)},
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	report := RunReport{}
	if _, err := conversation.Run(context.Background(), "unpersistable", testRuntime(model, 1), executor, LoopOptions{
		MaxTurns: 2, MaxTotalTokens: 1_000_000, Report: &report,
		// The turn cannot be made durable, so the batch never starts and the call
		// is closed as not executed.
		OnTurnCommitted: func(protocol.Turn) error { return errors.New("log unavailable") },
	}, false, nil); err == nil {
		t.Fatal("the persistence failure was not reported")
	}
	if item.calls.Load() != 0 {
		t.Fatalf("the tool ran %d times although the turn was not persisted", item.calls.Load())
	}
	// The closure is in the transcript, so the history stays usable, and the set
	// agrees with it rather than staying open.
	if state := stateOfCall(t, conversation, "one"); state != protocol.CallNotExecuted {
		t.Fatalf("state = %q, want not_executed", state)
	}
	if report.PendingAtEnd != 0 {
		t.Fatalf("pending at end = %d, want the closing path to have closed the set", report.PendingAtEnd)
	}
	if left := conversation.PendingCalls(); len(left) != 0 {
		t.Fatalf("the set outlived the request: %#v", left)
	}
}

// containsSubstring is strings.Contains under a name this file owns, so the test
// reads as an assertion about the message rather than about string plumbing.
func containsSubstring(haystack, needle string) bool {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}
