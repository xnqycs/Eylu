package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"Eylu/internal/environment"
	"Eylu/internal/protocol"
)

func TestStoreReplaysEventsAndHydratesAttachments(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	snapshot := createTestSession(t, store, "alpha", filepath.Join(root, "workspace-a"))
	content := strings.Repeat("attachment-marker-", AttachmentThreshold)
	turn := toolTurn("turn-tool", content)
	appended, err := store.Append(snapshot.SessionID, []Event{{Type: EventTurnAppended, Turn: &turn}})
	if err != nil {
		t.Fatal(err)
	}
	if appended[0].Turn.Parts[0].ToolResult.Content == content {
		t.Fatal("event retained an oversized tool result inline")
	}

	restarted := openTestStore(t, root)
	restored, diagnostics, err := restarted.Load(snapshot.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	if len(restored.Turns) != 1 || restored.Turns[0].Parts[0].ToolResult.Content != content {
		t.Fatal("attachment was not restored into the replayed turn")
	}
	if restored.Sequence != 2 {
		t.Fatalf("sequence = %d", restored.Sequence)
	}

	if err := restarted.Save(restored); err != nil {
		t.Fatal(err)
	}
	next := textTurn("turn-user", protocol.RoleUser, "continue")
	if _, err := restarted.Append(snapshot.SessionID, []Event{{Type: EventTurnAppended, Turn: &next}}); err != nil {
		t.Fatal(err)
	}
	replayed, _, err := openTestStore(t, root).Load(snapshot.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.Turns) != 2 || replayed.Turns[1].ID != "turn-user" || replayed.Sequence != 3 {
		t.Fatalf("replayed snapshot = %#v", replayed)
	}
}

func TestStoreReplaysEnvironmentFromCreationAndRuntimeEvents(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	created := environment.Context{WorkingDirectory: filepath.Join(root, "workspace"), Platform: "windows", Today: "2026-07-19"}
	snapshot, err := store.Create(Snapshot{SessionID: "environment", Workspace: created.WorkingDirectory, Environment: created, PermissionMode: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Sequence = 0
	snapshot.Environment = environment.Context{}
	if err := store.Save(snapshot); err != nil {
		t.Fatal(err)
	}
	replayed, _, err := store.Load("environment")
	if err != nil || replayed.Environment != created {
		t.Fatalf("creation environment = %#v, error = %v", replayed.Environment, err)
	}

	updated := created
	updated.Today = "2026-07-20"
	if _, err := store.Append("environment", []Event{{Type: EventRuntimeUpdated, Environment: &updated}}); err != nil {
		t.Fatal(err)
	}
	replayed, _, err = store.Load("environment")
	if err != nil || replayed.Environment != updated {
		t.Fatalf("updated environment = %#v, error = %v", replayed.Environment, err)
	}
}

func TestStoreReplaysAndClearsTodoListFromContextEvents(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	createTestSession(t, store, "todos", root)
	todos := protocol.TodoList{Explanation: "work", Items: []protocol.TodoItem{{ID: "implement", Content: "Implement tools", Status: protocol.TodoInProgress}}}
	if _, err := store.Append("todos", []Event{{Type: EventContextUpdated, TodoList: &todos}}); err != nil {
		t.Fatal(err)
	}
	replayed, _, err := store.Load("todos")
	if err != nil || len(replayed.TodoList.Items) != 1 || replayed.TodoList.Items[0].ID != "implement" {
		t.Fatalf("replayed=%#v err=%v", replayed.TodoList, err)
	}
	cleared := protocol.TodoList{Items: []protocol.TodoItem{}}
	if _, err := store.Append("todos", []Event{{Type: EventContextUpdated, TodoList: &cleared}}); err != nil {
		t.Fatal(err)
	}
	replayed, _, err = store.Load("todos")
	if err != nil || len(replayed.TodoList.Items) != 0 {
		t.Fatalf("cleared=%#v err=%v", replayed.TodoList, err)
	}
}

func TestStoreReplaysPromptHistoryEvents(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	createTestSession(t, store, "prompts", root)
	if _, err := store.Append("prompts", []Event{
		{Type: EventPromptRecorded, Prompt: "first"},
		{Type: EventPromptRecorded, Prompt: "first"},
		{Type: EventPromptRecorded, Prompt: "second\nline"},
	}); err != nil {
		t.Fatal(err)
	}
	replayed, _, err := store.Load("prompts")
	if err != nil || len(replayed.PromptHistory) != 3 || replayed.PromptHistory[0] != "first" || replayed.PromptHistory[1] != "first" || replayed.PromptHistory[2] != "second\nline" {
		t.Fatalf("history=%#v err=%v", replayed.PromptHistory, err)
	}
}

func TestSnapshotTodoListIsOptionalForSchemaVersionOne(t *testing.T) {
	encoded, err := json.Marshal(Snapshot{Version: SchemaVersion, SessionID: "legacy-v1", TodoList: protocol.TodoList{Items: []protocol.TodoItem{}}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"todo_list"`)) {
		t.Fatalf("empty todo list was serialized: %s", encoded)
	}
	var restored Snapshot
	if err := json.Unmarshal([]byte(`{"version":1,"session_id":"legacy-v1"}`), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Version != SchemaVersion || len(restored.TodoList.Items) != 0 {
		t.Fatalf("restored=%#v", restored)
	}
}

func TestStoreIgnoresDamagedJSONLTail(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	createTestSession(t, store, "recoverable", root)
	turn := textTurn("committed", protocol.RoleUser, "kept")
	if _, err := store.Append("recoverable", []Event{{Type: EventTurnAppended, Turn: &turn}}); err != nil {
		t.Fatal(err)
	}
	eventsPath := filepath.Join(root, "recoverable", "events.jsonl")
	file, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"version":1,"sequence":3`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	restarted := openTestStore(t, root)
	latest, found, err := restarted.Latest(root)
	if err != nil || !found || latest.SessionID != "recoverable" || !latest.Loadable || latest.Diagnostic == "" {
		t.Fatalf("latest = %#v, found = %t, error = %v", latest, found, err)
	}
	snapshot, diagnostics, err := restarted.Load("recoverable")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Turns) != 1 || snapshot.Turns[0].ID != "committed" {
		t.Fatalf("turns = %#v", snapshot.Turns)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	if sequence, err := lastEventSequence(eventsPath); err != nil || sequence != 2 {
		t.Fatalf("last sequence = %d, error = %v", sequence, err)
	}
	continued := textTurn("continued", protocol.RoleAgent, "after recovery")
	if _, err := store.Append("recoverable", []Event{{Type: EventTurnAppended, Turn: &continued}}); err != nil {
		t.Fatal(err)
	}
	recovered, diagnostics, err := openTestStore(t, root).Load("recoverable")
	if err != nil || len(diagnostics) != 0 || len(recovered.Turns) != 2 || recovered.Turns[1].ID != "continued" {
		t.Fatalf("recovered = %#v, diagnostics = %#v, error = %v", recovered, diagnostics, err)
	}
}

func TestStoreReportsTamperedAttachment(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	createTestSession(t, store, "tampered", root)
	content := strings.Repeat("sensitive-output", AttachmentThreshold)
	turn := toolTurn("tool", content)
	if _, err := store.Append("tampered", []Event{{Type: EventTurnAppended, Turn: &turn}}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "tampered", "attachments"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("attachments = %d, error = %v", len(entries), err)
	}
	if err := os.WriteFile(filepath.Join(root, "tampered", "attachments", entries[0].Name()), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, diagnostics, err := openTestStore(t, root).Load("tampered")
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0].Message, "mismatch") {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	if snapshot.Turns[0].Parts[0].ToolResult.Content == content {
		t.Fatal("tampered attachment content was trusted")
	}
}

func TestStoreRejectsOversizedAttachment(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	createTestSession(t, store, "oversized", root)
	turn := toolTurn("tool", strings.Repeat("x", MaxAttachmentBytes+1))
	if _, err := store.Append("oversized", []Event{{Type: EventTurnAppended, Turn: &turn}}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v", err)
	}
	loaded, diagnostics, err := store.Load("oversized")
	if err != nil || len(diagnostics) != 0 || len(loaded.Turns) != 0 || loaded.Sequence != 1 {
		t.Fatalf("loaded = %#v, diagnostics = %#v, error = %v", loaded, diagnostics, err)
	}
}

func TestStoreRejectsUnknownSchemaAndMigratesVersionZero(t *testing.T) {
	t.Run("unknown", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root)
		createTestSession(t, store, "future", root)
		path := filepath.Join(root, "future", "snapshot.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		document["version"] = SchemaVersion + 1
		data, _ = json.Marshal(document)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err = store.Load("future")
		if err == nil || !strings.Contains(err.Error(), "sessions migrate future") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("version-zero", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root)
		directory := filepath.Join(root, "legacy")
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		legacySnapshot := map[string]any{
			"version": 0, "sequence": 1, "session_id": "legacy", "created_at": now, "updated_at": now,
			"workspace": root, "permission_mode": "manual", "turns": []any{},
		}
		snapshotData, _ := json.Marshal(legacySnapshot)
		if err := os.WriteFile(filepath.Join(directory, "snapshot.json"), snapshotData, 0o600); err != nil {
			t.Fatal(err)
		}
		event := map[string]any{
			"version": 0, "sequence": 1, "type": EventSessionCreated, "session_id": "legacy", "at": now,
			"workspace": root, "permission_mode": "manual",
		}
		eventData, _ := json.Marshal(event)
		if err := os.WriteFile(filepath.Join(directory, "events.jsonl"), append(eventData, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Load("legacy"); err == nil {
			t.Fatal("legacy session loaded without explicit migration")
		}
		if err := store.Migrate("legacy"); err != nil {
			t.Fatal(err)
		}
		restored, _, err := store.Load("legacy")
		if err != nil || restored.Version != SchemaVersion {
			t.Fatalf("restored = %#v, error = %v", restored, err)
		}
		for _, name := range []string{"snapshot.json.v0.bak", "events.jsonl.v0.bak"} {
			if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
				t.Fatalf("missing migration backup %s: %v", name, err)
			}
		}
	})
}

func TestStoreIsolationListDeleteAndCleanup(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	for _, id := range []string{"one", "two", "three"} {
		createTestSession(t, store, id, filepath.Join(root, id))
		turn := textTurn("turn-"+id, protocol.RoleUser, id)
		if _, err := store.Append(id, []Event{{Type: EventTurnAppended, Turn: &turn}}); err != nil {
			t.Fatal(err)
		}
	}
	items, err := store.List()
	if err != nil || len(items) != 3 {
		t.Fatalf("items = %#v, error = %v", items, err)
	}
	for _, id := range []string{"one", "two", "three"} {
		snapshot, _, err := store.Load(id)
		if err != nil || len(snapshot.Turns) != 1 || snapshot.Turns[0].ID != "turn-"+id {
			t.Fatalf("session %s leaked state: %#v, %v", id, snapshot, err)
		}
	}
	deleted, err := store.Cleanup(2, 0, "two")
	if err != nil || len(deleted) != 1 || deleted[0] == "two" {
		t.Fatalf("deleted = %#v, error = %v", deleted, err)
	}
	if _, _, err := store.Load("two"); err != nil {
		t.Fatalf("protected session was removed: %v", err)
	}
	if err := store.Delete("../outside"); err == nil {
		t.Fatal("path traversal session ID was accepted")
	}
	if err := store.Delete("two"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load("two"); !os.IsNotExist(err) {
		t.Fatalf("deleted session load error = %v", err)
	}
}

func openTestStore(t *testing.T, root string) *Store {
	t.Helper()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func createTestSession(t *testing.T, store *Store, id, workspace string) Snapshot {
	t.Helper()
	snapshot, err := store.Create(Snapshot{
		SessionID: id, Workspace: workspace, PermissionMode: "auto",
		Provider: ProviderState{Name: "primary", Generation: 4, Adapter: "openai_responses", BaseURL: "https://example.test/v1", Model: "model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func textTurn(id string, role protocol.Role, text string) protocol.Turn {
	return protocol.Turn{ID: id, Role: role, Parts: []protocol.Part{{Kind: protocol.PartText, Text: text}}, CreatedAt: time.Now().UTC()}
}

func toolTurn(id, content string) protocol.Turn {
	return protocol.Turn{ID: id, Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: &protocol.ToolResult{CallID: "call", Content: content}}}, CreatedAt: time.Now().UTC()}
}
