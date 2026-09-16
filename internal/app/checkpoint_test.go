package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/session"
	"Eylu/internal/tool"
)

// writeDriver asks for one write_file call and then finishes.
type writeDriver struct {
	requests int
	path     string
	content  string
}

func (*writeDriver) Name() string { return "stub" }
func (*writeDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true}
}
func (d *writeDriver) Generate(_ context.Context, _ driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.requests++
	if d.requests == 1 {
		call := protocol.ToolCall{ID: "write-1", Name: "write_file", Arguments: json.RawMessage(`{"path":"` + d.path + `","content":"` + d.content + `","reason":"test"}`)}
		return protocol.ModelResponse{Turn: protocol.Turn{Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartToolCall, ToolCall: &call}}}, Stop: protocol.StopToolUse}, nil
	}
	return protocol.ModelResponse{Turn: protocol.Turn{Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "done"}}}, Stop: protocol.StopCompleted}, nil
}

// runInterruptedRun drives one request whose lifecycle records can be made to
// fail at a chosen point, then abandons the runtime as a crash would.
func runInterruptedRun(t *testing.T, fixture *sessionSyncFixture, driver2 *writeDriver, failIntent, failCompletion bool) error {
	t.Helper()
	fixture.controller.appendEvents = func(id string, events []session.Event) ([]session.Event, error) {
		for _, event := range events {
			switch {
			case event.Type == session.EventToolExecutionIntent && failIntent:
				return nil, errors.New("intent record failed")
			case event.Type == session.EventToolCompleted && failCompletion:
				return nil, errors.New("completion record failed")
			}
		}
		return fixture.store.Append(id, events)
	}
	write, err := tool.NewWriteFile(fixture.workspace)
	if err != nil {
		t.Fatal(err)
	}
	executor := &tool.Executor{
		Registry: tool.NewRegistry(write), Policy: policy.AllowAllChecker{},
		Checkpoint: fixture.controller, Workspace: fixture.workspace,
	}
	conversation := conversationWithPrompts(t, fixture.sessionID)
	runtime := agent.Runtime{
		Provider: provider.Snapshot{Name: "stub", Config: config.ProviderConfig{Adapter: "stub", BaseURL: "https://example.test/v1", Model: "stub-model"}},
		Driver:   driver2, Workspace: fixture.workspace, PermissionMode: "full",
	}
	_, err = conversation.Run(context.Background(), "write the file", runtime, executor, agent.LoopOptions{MaxTurns: 3, MaxTotalTokens: 1_000_000}, false, nil)
	return err
}

// reload reads the session from the log alone, which is what a restart does.
func reload(t *testing.T, fixture *sessionSyncFixture) session.Snapshot {
	t.Helper()
	store, err := session.Open(fixture.store.Root())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, diagnostics, err := store.LoadRecovering(fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	return snapshot
}

// A run that dies before its intent is recorded never touched the file.
func TestInterruptedRunBeforeIntentLeavesNothingBehind(t *testing.T) {
	fixture := newSessionSyncFixture(t)
	err := runInterruptedRun(t, fixture, &writeDriver{path: "target.txt", content: "written"}, true, false)
	if err == nil {
		t.Fatal("expected the intent failure to be reported")
	}
	if _, statErr := os.Stat(filepath.Join(fixture.workspace, "target.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("the file was written although its intent failed")
	}
	events := fixture.events(t)
	if countEvents(events, session.EventToolExecutionIntent) != 0 || countEvents(events, session.EventToolCompleted) != 0 {
		t.Fatalf("lifecycle events were recorded: %#v", events)
	}
	if pending := reload(t, fixture).PendingIntents; len(pending) != 0 {
		t.Fatalf("pending intents = %#v", pending)
	}
}

// A run that dies after its intent but before the result record leaves evidence:
// the file was written, the log proves the operation was started, and recovery
// reports it as unknown instead of repeating it.
func TestInterruptedRunAfterIntentLeavesRecoverableEvidence(t *testing.T) {
	fixture := newSessionSyncFixture(t)
	err := runInterruptedRun(t, fixture, &writeDriver{path: "target.txt", content: "written"}, false, true)
	if err == nil {
		t.Fatal("expected the completion failure to be reported")
	}
	data, readErr := os.ReadFile(filepath.Join(fixture.workspace, "target.txt"))
	if readErr != nil || string(data) != "written" {
		t.Fatalf("the side effect did not happen: %q err=%v", data, readErr)
	}
	events := fixture.events(t)
	if countEvents(events, session.EventToolExecutionIntent) != 1 || countEvents(events, session.EventToolCompleted) != 0 {
		t.Fatalf("lifecycle events = %#v", events)
	}
	snapshot := reload(t, fixture)
	if len(snapshot.PendingIntents) != 1 {
		t.Fatalf("pending intents = %#v", snapshot.PendingIntents)
	}
	intent := snapshot.PendingIntents[0]
	if intent.Tool != "write_file" || filepath.Base(intent.TargetPath) != "target.txt" {
		t.Fatalf("intent = %#v", intent)
	}
	// The recorded hint is the state before the write, so a human can tell the
	// file changed since: it did not exist before, and it exists now.
	if intent.PreviousHash != "" {
		t.Fatalf("unexpected previous hash for a new file: %q", intent.PreviousHash)
	}
	// Recovery never replays the operation: the reloaded transcript holds no
	// unanswered call and the file is untouched by the reload itself.
	before := string(data)
	if again, err := os.ReadFile(filepath.Join(fixture.workspace, "target.txt")); err != nil || string(again) != before {
		t.Fatalf("recovery replayed the write: %q", again)
	}
	if _, err := agent.RestoreConversation(agentStateFromSnapshot(snapshot)); err != nil {
		t.Fatalf("restore = %v", err)
	}
}

// A completed run records both sides, so a later load has nothing pending and the
// snapshot may lag behind the log.
func TestCompletedRunHasNoPendingIntentEvenWithoutASnapshot(t *testing.T) {
	fixture := newSessionSyncFixture(t)
	if err := runInterruptedRun(t, fixture, &writeDriver{path: "target.txt", content: "written"}, false, false); err != nil {
		t.Fatalf("err = %v", err)
	}
	events := fixture.events(t)
	if countEvents(events, session.EventToolExecutionIntent) != 1 || countEvents(events, session.EventToolCompleted) != 1 {
		t.Fatalf("lifecycle events = %#v", events)
	}
	// The snapshot was never written by this test, so the log alone must be enough.
	if err := os.Remove(filepath.Join(fixture.store.Root(), fixture.sessionID, "snapshot.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	snapshot := reload(t, fixture)
	if len(snapshot.PendingIntents) != 0 {
		t.Fatalf("pending intents = %#v", snapshot.PendingIntents)
	}
	data, err := os.ReadFile(filepath.Join(fixture.workspace, "target.txt"))
	if err != nil || string(data) != "written" {
		t.Fatalf("file = %q err = %v", data, err)
	}
}

// A checkpoint callback that reads the conversation cannot deadlock, because the
// executor runs with the state lock released.
func TestCheckpointDoesNotReenterTheStateLock(t *testing.T) {
	workspace := t.TempDir()
	conversation := agent.NewConversation()
	write, err := tool.NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	sink := &readingCheckpoint{read: func() {
		reads++
		_ = conversation.ContextReport()
		_ = conversation.ExportState()
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(write), Policy: policy.AllowAllChecker{}, Checkpoint: sink, Workspace: workspace}
	runtime := agent.Runtime{
		Provider: provider.Snapshot{Name: "stub", Config: config.ProviderConfig{Adapter: "stub", BaseURL: "https://example.test/v1", Model: "stub-model"}},
		Driver:   &writeDriver{path: "target.txt", content: "written"}, Workspace: workspace, PermissionMode: "full",
	}
	done := make(chan error, 1)
	go func() {
		_, err := conversation.Run(context.Background(), "write", runtime, executor, agent.LoopOptions{MaxTurns: 3, MaxTotalTokens: 1_000_000}, false, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a checkpoint callback that reads the conversation deadlocked the request")
	}
	if reads != 2 {
		t.Fatalf("the checkpoint observed %d lifecycle points, want the intent and the completion", reads)
	}
}

type readingCheckpoint struct{ read func() }

func (c *readingCheckpoint) RecordIntent(tool.Intent) error {
	c.read()
	return nil
}
func (c *readingCheckpoint) RecordCompletion(tool.Completion) error {
	c.read()
	return nil
}
