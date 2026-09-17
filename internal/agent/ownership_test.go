package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	contextledger "Eylu/internal/context"
	"Eylu/internal/environment"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

// withinFailBudget runs one action and reports whether it completed in time. A
// state operation that waits for a model call does not complete at all, so the
// bound is what turns the wait into a failure instead of a hung suite.
func withinFailBudget(t *testing.T, what string, action func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		action()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s waited for a model call", what)
	}
}

// A whole-conversation operation owns the conversation from the moment its wait
// ends, and it keeps owning it until it is done.
//
// Waiting without claiming leaves a gap: a request that starts in it appends to
// the session the rotation is about to replace, and its work is then thrown away.
// The claim is what makes "the previous request finished" and "the rotation owns
// the state" one indivisible step.
func TestGateAcquireKeepsRequestsOutForTheWholeRotation(t *testing.T) {
	var gate runGate
	if !gate.idle() {
		t.Fatal("a fresh gate is not idle")
	}
	gate.acquire()
	if err := gate.begin(); !errors.Is(err, ErrConversationBusy) {
		t.Fatalf("a request started while the conversation was being rotated: err = %v", err)
	}
	gate.release()
	if err := gate.begin(); err != nil {
		t.Fatalf("the conversation stayed locked after the rotation: %v", err)
	}
	gate.release()

	// A rotation that arrives while a request runs waits for it, and a second
	// rotation waits for the first instead of interleaving with it.
	if err := gate.begin(); err != nil {
		t.Fatal(err)
	}
	acquired := make(chan struct{})
	go func() {
		gate.acquire()
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("a rotation acquired a conversation that was still running a request")
	case <-time.After(100 * time.Millisecond):
	}
	gate.release()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("the rotation never acquired the conversation the request released")
	}
	gate.release()
}

// A rotation waits for a request that is blocked in a model call, and then owns
// the conversation: the new session is empty and a request that follows runs in
// it.
func TestRotationWaitsForABlockedRequestAndThenOwnsTheConversation(t *testing.T) {
	model := &blockingDriver{started: make(chan struct{}), release: make(chan struct{})}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	originalID := conversation.SessionID()
	completed := make(chan error, 1)
	go func() {
		_, err := conversation.Run(context.Background(), "first", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 1, MaxTotalTokens: 1_000_000}, false, nil)
		completed <- err
	}()
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not start")
	}
	rotated := make(chan string, 1)
	go func() { rotated <- conversation.NewSessionWithEnvironment(environment.Context{}) }()
	close(model.release)
	if err := <-completed; err != nil {
		t.Fatalf("err = %v", err)
	}
	select {
	case id := <-rotated:
		if id != originalID {
			t.Fatalf("rotation returned %q, want the previous session %q", id, originalID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the rotation never completed")
	}
	if conversation.SessionID() == originalID {
		t.Fatal("the rotation did not change the session")
	}
	if turns := conversation.Transcript(); len(turns) != 0 {
		t.Fatalf("the rotated conversation kept %d turn(s): %#v", len(turns), turns)
	}
	// The previous session is what a reader of the old log still sees.
	if turns, ok := conversation.ClosedTranscript(originalID); !ok || len(turns) == 0 {
		t.Fatalf("the previous session was lost: ok=%t turns=%d", ok, len(turns))
	}
}

// Reading the conversation, exporting it and stopping the request must not wait
// for a compaction summary that is still being produced.
//
// The summary is a model call, so holding the state lock across it makes the
// conversation unreadable and unstoppable for as long as the provider takes. The
// stop path is the one that matters most: a host that narrows a safety setting
// during a long summary has to be able to say so.
func TestStateReadsAndAStopRequestDoNotWaitForTheCompactionSummary(t *testing.T) {
	summary := &blockingDriver{started: make(chan struct{}), release: make(chan struct{})}
	conversation := compactableConversation()
	runtime := compactionUsageRuntime(summary)
	request := make(chan error, 1)
	go func() {
		_, err := conversation.Send(context.Background(), "compact", runtime, false, nil)
		request <- err
	}()
	select {
	case <-summary.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the summary call never started")
	}

	withinFailBudget(t, "reading the conversation state", func() {
		if report := conversation.ContextReport(); report.InputTokens < 0 {
			t.Error("the report was not readable")
		}
		_ = conversation.ExportState()
	})
	withinFailBudget(t, "stopping the request", func() {
		conversation.RequestStop("a safety setting was narrowed")
	})

	close(summary.release)
	select {
	case <-request:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never finished")
	}
}

// A manual compaction delivers its host callbacks with the state lock released,
// so a callback that reads the conversation cannot deadlock against the
// compaction that produced it.
func TestManualCompactionDeliversCallbacksOutsideTheStateLock(t *testing.T) {
	conversation := compactableConversation()
	runtime := compactionUsageRuntime(&compactionSummaryDriver{})
	events := make([]contextledger.Event, 0, 4)
	runtime.ContextEvent = func(event contextledger.Event) {
		events = append(events, event)
		// A host callback is allowed to read the conversation.
		_ = conversation.ContextReport()
		_ = conversation.ExportState()
	}
	var event contextledger.CompressionEvent
	var err error
	withinFailBudget(t, "a manual compaction callback", func() {
		event, err = conversation.Compact(context.Background(), runtime)
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if event.Strategy != "model" || event.OmittedTurns == 0 {
		t.Fatalf("event = %#v", event)
	}
	if len(events) == 0 {
		t.Fatal("the host was told nothing about the compaction")
	}
}

// A compaction whose state moved on while the summary was being produced is
// rejected rather than committed: the summary describes turns the conversation no
// longer has.
func TestACompactionDecidedOnAChangedStateIsRejected(t *testing.T) {
	conversation := compactableConversation()
	runtime := compactionUsageRuntime(&compactionSummaryDriver{})
	options := optionsForRuntime(runtime)
	prepared := conversation.buildPromptContext(runtime, nil)
	_, _, plan, err := conversation.planCompactionLocked(runtime, nil, options, prepared, "auto", false)
	if err != nil || plan == nil {
		t.Fatalf("plan = %#v err = %v", plan, err)
	}
	// The conversation moves on before the summary is committed.
	conversation.turns = append(conversation.turns, protocol.Turn{
		ID: "late", Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "a newer goal"}},
	})
	if _, event, err := conversation.runCompaction(context.Background(), *plan); err != nil {
		t.Fatalf("err = %v", err)
	} else if !event.Noop {
		t.Fatalf("a compaction decided on a superseded state was committed: %#v", event)
	}
	if conversation.summary != "" {
		t.Fatalf("the superseded summary was committed: %q", conversation.summary)
	}
	if len(conversation.omittedTurnIDs) != 0 {
		t.Fatalf("turns were omitted from a superseded decision: %#v", conversation.omittedTurnIDs)
	}
}

// Cancelling, rotating and running again must keep working for as many rounds as
// a session is used, and the state must stay consistent throughout.
func TestRepeatedCancelRotateAndRunCycles(t *testing.T) {
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	sessions := make([]string, 0, 4)
	sessions = append(sessions, conversation.SessionID())
	for round := 0; round < 3; round++ {
		model := &blockingDriver{started: make(chan struct{}), release: make(chan struct{})}
		completed := make(chan error, 1)
		go func() {
			_, err := conversation.Run(context.Background(), "cycle", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 1, MaxTotalTokens: 1_000_000}, false, nil)
			completed <- err
		}()
		select {
		case <-model.started:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: the request did not start", round)
		}
		conversation.CancelRun()
		select {
		case <-completed:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: the cancelled request never returned", round)
		}
		if pending := conversation.OpenPendingCalls(); len(pending) != 0 {
			t.Fatalf("round %d: the request left its pending set behind: %#v", round, pending)
		}
		previous := conversation.SessionID()
		if rotated := conversation.NewSession(); rotated != previous {
			t.Fatalf("round %d: rotation returned %q, want %q", round, rotated, previous)
		}
		if current := conversation.SessionID(); current == previous {
			t.Fatalf("round %d: the rotation did not change the session", round)
		} else {
			sessions = append(sessions, current)
		}
		if len(conversation.Transcript()) != 0 {
			t.Fatalf("round %d: the new session is not empty", round)
		}
	}
	// Every session this conversation left behind is still readable, and each one
	// belongs to exactly one round.
	seen := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		if seen[session] {
			t.Fatalf("session %q was reused", session)
		}
		seen[session] = true
	}
}
