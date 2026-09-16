package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A started execution is pending until its terminal outcome is recorded; the
// pending set is what recovery reports as unknown.
func TestToolLifecycleEventsTrackPendingIntents(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(Snapshot{SessionID: "lifecycle"}); err != nil {
		t.Fatal(err)
	}
	intent := ToolIntent{
		RequestID: "request-1", CallID: "exec-1", ParentCallID: "call-1", Tool: "write_file",
		TargetPath: "/workspace/target.txt", PreviousHash: "hash-before", StartedAt: time.Now().UTC(),
	}
	if _, err := store.Append("lifecycle", []Event{{Type: EventToolExecutionIntent, Intent: &intent}}); err != nil {
		t.Fatal(err)
	}
	snapshot, diagnostics, err := store.Load("lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	if len(snapshot.PendingIntents) != 1 {
		t.Fatalf("pending intents = %#v", snapshot.PendingIntents)
	}
	recorded := snapshot.PendingIntents[0]
	if recorded.CallID != "exec-1" || recorded.Tool != "write_file" || recorded.TargetPath != "/workspace/target.txt" || recorded.PreviousHash != "hash-before" {
		t.Fatalf("recorded intent = %#v", recorded)
	}

	completion := ToolCompletion{CallID: "exec-1", Tool: "write_file", State: "succeeded", TargetPath: "/workspace/target.txt", ResultHash: "hash-after", CompletedAt: time.Now().UTC()}
	if _, err := store.Append("lifecycle", []Event{{Type: EventToolCompleted, Completion: &completion}}); err != nil {
		t.Fatal(err)
	}
	reloaded, _, err := store.Load("lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.PendingIntents) != 0 {
		t.Fatalf("a completed execution stayed pending: %#v", reloaded.PendingIntents)
	}
}

// A log written by the previous schema version is still readable, and new events
// are written with the current version.
func TestVersion2SessionStillLoads(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(store.Root(), "legacy-v2")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	snapshotBody, err := json.Marshal(map[string]any{
		"version": 2, "session_id": "legacy-v2", "sequence": 2, "prompt_history": []string{"old"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "snapshot.json"), snapshotBody, 0o600); err != nil {
		t.Fatal(err)
	}
	events := []string{
		`{"version":2,"sequence":1,"type":"session_created","session_id":"legacy-v2","at":"2026-01-01T00:00:00Z"}`,
		`{"version":2,"sequence":2,"type":"prompt_recorded","session_id":"legacy-v2","at":"2026-01-01T00:00:01Z","prompt":"old"}`,
	}
	if err := os.WriteFile(filepath.Join(directory, "events.jsonl"), []byte(strings.Join(events, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, diagnostics, err := reopened.Load("legacy-v2")
	if err != nil {
		t.Fatalf("a version %d session was refused: %v", MinReadableSchemaVersion, err)
	}
	if len(diagnostics) != 0 || len(snapshot.PromptHistory) != 1 {
		t.Fatalf("snapshot=%#v diagnostics=%#v", snapshot, diagnostics)
	}
	prepared, err := reopened.Append("legacy-v2", []Event{{Type: EventPromptRecorded, Prompt: "new"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 1 || prepared[0].Version != SchemaVersion {
		t.Fatalf("new event version = %#v", prepared)
	}
}

// A document from a version this build cannot read is refused on load, and
// Migrate brings it forward with a backup of the original.
func TestOlderSchemaIsRefusedOnLoadAndMigratedWithABackup(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(store.Root(), "ancient")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"version":1,"session_id":"ancient"}`)
	snapshotPath := filepath.Join(directory, "snapshot.json")
	if err := os.WriteFile(snapshotPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load("ancient"); err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Fatalf("err = %v", err)
	}
	if err := store.Migrate("ancient"); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(snapshotPath + ".v1.bak")
	if err != nil {
		t.Fatalf("the original document was not backed up: %v", err)
	}
	if string(backup) != string(original) {
		t.Fatalf("backup = %s", backup)
	}
	migrated, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(migrated), fmt.Sprintf(`"version": %d`, SchemaVersion)) {
		t.Fatalf("migrated = %s", migrated)
	}
	// A version this build does not know is refused even by Migrate.
	unknown := []byte(fmt.Sprintf(`{"version":%d,"session_id":"ancient"}`, SchemaVersion+1))
	if err := os.WriteFile(snapshotPath, unknown, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate("ancient"); err == nil || !strings.Contains(err.Error(), "unsupported session schema version") {
		t.Fatalf("err = %v", err)
	}
}
