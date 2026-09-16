package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	"Eylu/internal/environment"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/session"
)

// sessionSyncFixture wires a real store, a provider manager and a conversation
// whose transcript can grow independently of a model.
type sessionSyncFixture struct {
	store      *session.Store
	manager    *provider.Manager
	controller *sessionRuntime
	workspace  string
	sessionID  string
}

func newSessionSyncFixture(t *testing.T) *sessionSyncFixture {
	t.Helper()
	isolateUserState(t)
	workspace := t.TempDir()
	cfg := config.Default()
	cfg.ActiveProvider = "saved"
	cfg.Providers["saved"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: "https://example.test/v1", Model: "saved-model"}
	manager, err := provider.NewManager(filepath.Join(workspace, "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	providerSnapshot, err := manager.Active()
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.Open("")
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(session.Snapshot{
		SessionID: "sync-session", Workspace: workspace, Environment: environment.Context{WorkingDirectory: workspace},
		PermissionMode: "manual",
		Provider: session.ProviderState{
			Name: providerSnapshot.Name, Generation: providerSnapshot.Generation,
			Adapter: "openai_responses", BaseURL: "https://example.test/v1", Model: "saved-model",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &sessionSyncFixture{
		store: store, manager: manager, workspace: workspace, sessionID: created.SessionID,
		controller: newSessionRuntime(store, created, workspace, nil),
	}
}

// conversationWithPrompts restores a conversation holding one user/agent pair and
// one recorded prompt per argument.
func conversationWithPrompts(t *testing.T, sessionID string, prompts ...string) *agent.Conversation {
	t.Helper()
	state := agent.NewConversation().ExportState()
	state.SessionID = sessionID
	state.Turns = nil
	state.PromptHistory = nil
	for index, prompt := range prompts {
		state.Turns = append(state.Turns,
			protocol.Turn{ID: fmt.Sprintf("user-%d", index), Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: prompt}}},
			protocol.Turn{ID: fmt.Sprintf("agent-%d", index), Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "answer-" + prompt}}},
		)
		state.PromptHistory = append(state.PromptHistory, prompt)
	}
	conversation, err := agent.RestoreConversation(state)
	if err != nil {
		t.Fatal(err)
	}
	return conversation
}

func (f *sessionSyncFixture) events(t *testing.T) []session.Event {
	t.Helper()
	path := filepath.Join(f.store.Root(), f.sessionID, "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	events := make([]session.Event, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event session.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode event %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func countEvents(events []session.Event, eventType session.EventType) int {
	count := 0
	for _, event := range events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}

// A confirmed append must not be repeated when the following snapshot save
// fails: the log keeps one event per logical change.
func TestSyncDoesNotReappendAfterSnapshotFailure(t *testing.T) {
	fixture := newSessionSyncFixture(t)
	failures := 2
	fixture.controller.saveSnapshot = func(next session.Snapshot) error {
		if failures > 0 {
			failures--
			return errors.New("snapshot write failed")
		}
		return fixture.store.Save(next)
	}
	conversation := conversationWithPrompts(t, fixture.sessionID, "first")

	first := fixture.events(t)
	if err := fixture.controller.Sync(conversation, fixture.manager, chatOptions{}, nil); err == nil {
		t.Fatal("expected the snapshot failure to be reported")
	}
	if !fixture.controller.snapshotPending {
		t.Fatal("a failed save did not mark the snapshot as pending")
	}
	afterFirst := fixture.events(t)
	turns := countEvents(afterFirst, session.EventTurnAppended)
	if turns != 2 || len(afterFirst) == len(first) {
		t.Fatalf("first sync turns = %d events = %d", turns, len(afterFirst))
	}
	// The second attempt still fails, so the log must not grow at all.
	if err := fixture.controller.Sync(conversation, fixture.manager, chatOptions{}, nil); err == nil {
		t.Fatal("expected the second snapshot failure to be reported")
	}
	if afterSecond := fixture.events(t); len(afterSecond) != len(afterFirst) {
		t.Fatalf("a failed snapshot re-appended events: %d -> %d", len(afterFirst), len(afterSecond))
	}

	// Once the save succeeds, the events already confirmed are not repeated and
	// only genuinely new ones are appended.
	grown := conversationWithPrompts(t, fixture.sessionID, "first", "second")
	if err := fixture.controller.Sync(grown, fixture.manager, chatOptions{}, nil); err != nil {
		t.Fatalf("third sync: %v", err)
	}
	final := fixture.events(t)
	if got := countEvents(final, session.EventTurnAppended); got != 4 {
		t.Fatalf("turn events = %d, want one per turn", got)
	}
	if got := countEvents(final, session.EventPromptRecorded); got != 2 {
		t.Fatalf("prompt events = %d, want one per prompt", got)
	}
	if got := countEvents(final, session.EventRuntimeUpdated); got != 1 {
		t.Fatalf("runtime events = %d, want one for the unchanged state", got)
	}
	if got := countEvents(final, session.EventContextUpdated); got != 1 {
		t.Fatalf("context events = %d, want one for the unchanged state", got)
	}
	if fixture.controller.snapshotPending {
		t.Fatal("snapshot is still pending after a successful save")
	}
	// The persisted snapshot matches the log.
	snapshot, _, err := fixture.store.Load(fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Turns) != 4 || len(snapshot.PromptHistory) != 2 || snapshot.Sequence != fixture.controller.log.sequence {
		t.Fatalf("snapshot turns=%d prompts=%d sequence=%d log=%d", len(snapshot.Turns), len(snapshot.PromptHistory), snapshot.Sequence, fixture.controller.log.sequence)
	}
}

// A state event is appended only while its payload keeps changing, so a retry
// after a failed save never duplicates it.
func TestSyncAppendsStateEventsOnlyWhenTheyChange(t *testing.T) {
	fixture := newSessionSyncFixture(t)
	conversation := conversationWithPrompts(t, fixture.sessionID, "first")
	if err := fixture.controller.Sync(conversation, fixture.manager, chatOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	before := fixture.events(t)
	if got := countEvents(before, session.EventDriverState); got != 0 {
		t.Fatalf("driver state events = %d, want none for an empty state", got)
	}

	// A driver state change is appended exactly once.
	state := conversation.ExportState()
	state.DriverState = json.RawMessage(`{"response_id":"one"}`)
	withState, err := agent.RestoreConversation(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.controller.Sync(withState, fixture.manager, chatOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := countEvents(fixture.events(t), session.EventDriverState); got != 1 {
		t.Fatalf("driver state events = %d, want 1", got)
	}
	// Re-syncing the same state appends nothing new.
	marker := len(fixture.events(t))
	if err := fixture.controller.Sync(withState, fixture.manager, chatOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(fixture.events(t)); got != marker {
		t.Fatalf("unchanged state appended %d extra events", got-marker)
	}
}

// Recovery from the log alone keeps the conversation usable: the snapshot can be
// deleted after a failed save and the replayed log still holds every turn once.
func TestSyncLogAloneRecoversAfterSnapshotFailure(t *testing.T) {
	fixture := newSessionSyncFixture(t)
	fixture.controller.saveSnapshot = func(session.Snapshot) error { return errors.New("snapshot write failed") }
	conversation := conversationWithPrompts(t, fixture.sessionID, "first", "second")
	if err := fixture.controller.Sync(conversation, fixture.manager, chatOptions{}, nil); err == nil {
		t.Fatal("expected the snapshot failure to be reported")
	}
	if err := os.Remove(filepath.Join(fixture.store.Root(), fixture.sessionID, "snapshot.json")); err != nil {
		t.Fatal(err)
	}
	recovered, diagnostics, err := fixture.store.LoadRecovering(fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	if len(recovered.Turns) != 4 || len(recovered.PromptHistory) != 2 {
		t.Fatalf("replayed turns=%d prompts=%d", len(recovered.Turns), len(recovered.PromptHistory))
	}
	seen := make(map[string]int)
	for _, turn := range recovered.Turns {
		seen[turn.ID]++
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("replayed turn %q appears %d times", id, count)
		}
	}
	restored, err := agent.RestoreConversation(agentStateFromSnapshot(recovered))
	if err != nil {
		t.Fatal(err)
	}
	if restored.SessionID() != fixture.sessionID {
		t.Fatalf("restored session = %q", restored.SessionID())
	}
}
