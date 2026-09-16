package app

import (
	"testing"

	"Eylu/internal/agent"
	"Eylu/internal/session"
	"Eylu/internal/testfault"
)

// eventIDCounts maps every event ID in the log to the number of times it appears.
func eventIDCounts(t *testing.T, fixture *sessionSyncFixture) map[string]int {
	t.Helper()
	counts := make(map[string]int)
	for _, event := range fixture.events(t) {
		if event.ID == "" {
			t.Fatal("an appended event has no stable ID")
		}
		counts[event.ID]++
	}
	return counts
}

// A turn written while the request runs is not written again when the request
// finishes and Sync replays the conversation: the identity is the turn ID, so
// both paths produce the same event and the log recognizes the retry.
func TestIncrementalTurnWriteAndSyncKeepOneCopyOfEachTurn(t *testing.T) {
	fixture := newSessionSyncFixture(t)
	conversation := conversationWithPrompts(t, fixture.sessionID, "first")

	turns := conversation.ExportState().Turns
	for _, turn := range turns {
		if err := fixture.controller.RecordTurn(turn); err != nil {
			t.Fatal(err)
		}
	}
	incremental := fixture.events(t)
	if got := countEvents(incremental, session.EventTurnAppended); got != len(turns) {
		t.Fatalf("turn events = %d, want %d", got, len(turns))
	}
	if !fixture.controller.snapshotPending {
		t.Fatal("the snapshot is not marked as behind the log")
	}

	if err := fixture.controller.Sync(conversation, fixture.manager, chatOptions{}, nil); err != nil {
		t.Fatalf("sync: %v", err)
	}
	final := fixture.events(t)
	if got := countEvents(final, session.EventTurnAppended); got != len(turns) {
		t.Fatalf("turn events = %d after the sync, want one per turn", got)
	}
	for id, count := range eventIDCounts(t, fixture) {
		if count != 1 {
			t.Fatalf("event ID %q appears %d times", id, count)
		}
	}
	// The log alone rebuilds the conversation, and the snapshot agrees with it.
	snapshot := reload(t, fixture)
	if len(snapshot.Turns) != len(turns) || len(snapshot.PromptHistory) != 1 {
		t.Fatalf("replayed turns = %d prompts = %d", len(snapshot.Turns), len(snapshot.PromptHistory))
	}
	if _, err := agent.RestoreConversation(agentStateFromSnapshot(snapshot)); err != nil {
		t.Fatalf("restore = %v", err)
	}
}

// A turn written while the request runs survives a crash that happens before the
// request ends: there is no Sync and no snapshot, and the log still holds it.
func TestIncrementallyWrittenTurnSurvivesACrashWithoutASync(t *testing.T) {
	fixture := newSessionSyncFixture(t)
	conversation := conversationWithPrompts(t, fixture.sessionID, "first")
	turns := conversation.ExportState().Turns

	if err := fixture.controller.RecordTurn(turns[0]); err != nil {
		t.Fatal(err)
	}
	// The process dies here.
	snapshot := reload(t, fixture)
	if len(snapshot.Turns) != 1 || snapshot.Turns[0].ID != turns[0].ID {
		t.Fatalf("recovered turns = %#v", snapshot.Turns)
	}

	// The next process continues from the log and does not repeat the turn it
	// already holds, even though its identity is the same.
	restarted := newSessionRuntime(fixture.store, snapshot, fixture.workspace, nil)
	if err := restarted.RecordTurn(turns[0]); err != nil {
		t.Fatal(err)
	}
	if got := countEvents(fixture.events(t), session.EventTurnAppended); got != 1 {
		t.Fatalf("the same turn was written %d times", got)
	}
}

// A turn whose record cannot be written is reported, and the request is told so
// instead of continuing as if the conversation were durable.
func TestRecordTurnReportsAFailedAppend(t *testing.T) {
	fixture := newSessionSyncFixture(t)
	conversation := conversationWithPrompts(t, fixture.sessionID, "first")
	faults := &testfault.StoreFaults{
		Delegate:    fixture.store,
		AppendFault: testfault.NewFault("store.Append", testfault.Always()),
	}
	installStore(fixture.controller, faults)

	if err := fixture.controller.RecordTurn(conversation.ExportState().Turns[0]); err == nil {
		t.Fatal("a failed turn append was reported as success")
	}
	if got := countEvents(fixture.events(t), session.EventTurnAppended); got != 0 {
		t.Fatalf("a failed append still wrote %d turns", got)
	}
	// The progress tracker did not advance, so the next Sync still writes the
	// turn rather than skipping it.
	if fixture.controller.log.turns != 0 {
		t.Fatalf("append progress advanced to %d after a failure", fixture.controller.log.turns)
	}
}
