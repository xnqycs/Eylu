package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func appendEvent(t *testing.T, store *Store, id, eventID, prompt string) []Event {
	t.Helper()
	prepared, err := store.Append(id, []Event{{ID: eventID, Type: EventPromptRecorded, Prompt: prompt}})
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func logLines(t *testing.T, store *Store, id string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(store.Root(), id, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := make([]string, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// Re-sending one logical event with the same ID is a retry, not a new event.
func TestAppendTreatsARepeatedEventIDAsARetry(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(Snapshot{SessionID: "retry", Provider: ProviderState{Name: "provider"}}); err != nil {
		t.Fatal(err)
	}
	first := appendEvent(t, store, "retry", "event-one", "hello")
	if len(first) != 1 || first[0].Sequence == 0 {
		t.Fatalf("prepared = %#v", first)
	}
	before := len(logLines(t, store, "retry"))

	second := appendEvent(t, store, "retry", "event-one", "hello")
	if len(second) != 1 || second[0].Sequence != first[0].Sequence {
		t.Fatalf("the retry was not reported as the stored event: %#v", second)
	}
	if after := len(logLines(t, store, "retry")); after != before {
		t.Fatalf("the retry wrote a second copy: %d -> %d lines", before, after)
	}
	// A genuinely new event still gets a fresh sequence.
	third := appendEvent(t, store, "retry", "event-two", "again")
	if len(third) != 1 || third[0].Sequence != first[0].Sequence+1 {
		t.Fatalf("new event sequence = %#v", third)
	}
	if after := len(logLines(t, store, "retry")); after != before+1 {
		t.Fatalf("lines = %d", after)
	}
}

// The same ID carrying different content is a conflict, not a retry.
func TestAppendRefusesTheSameEventIDWithDifferentContent(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(Snapshot{SessionID: "conflict"}); err != nil {
		t.Fatal(err)
	}
	appendEvent(t, store, "conflict", "event-one", "hello")
	before := logLines(t, store, "conflict")

	_, err = store.Append("conflict", []Event{{ID: "event-one", Type: EventPromptRecorded, Prompt: "something else"}})
	if err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("err = %v", err)
	}
	if after := logLines(t, store, "conflict"); len(after) != len(before) {
		t.Fatalf("a conflicting event was written: %d -> %d", len(before), len(after))
	}
}

// When the write result is uncertain, the cached tail is dropped so the next
// attempt re-reads the log instead of continuing from a sequence the log may not
// have.
func TestAppendRecoversTheLogTailAfterAnUncertainWrite(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(Snapshot{SessionID: "uncertain"}); err != nil {
		t.Fatal(err)
	}
	appendEvent(t, store, "uncertain", "event-one", "hello")
	eventsPath := filepath.Join(store.Root(), "uncertain", "events.jsonl")
	original, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	// A directory at the log path makes the write fail.
	if err := os.Remove(eventsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(eventsPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("uncertain", []Event{{ID: "event-two", Type: EventPromptRecorded, Prompt: "lost"}}); err == nil {
		t.Skip("this platform allows appending to a directory")
	}
	// The tail is restored exactly as the log had it, so the failed attempt wrote
	// nothing and the next append continues from the real sequence.
	if err := os.Remove(eventsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eventsPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	prepared := appendEvent(t, store, "uncertain", "event-two", "kept")
	// The session_created event is sequence 1 and the first append is sequence 2,
	// so the recovered append continues at 3 without a gap.
	if len(prepared) != 1 || prepared[0].Sequence != 3 {
		t.Fatalf("prepared = %#v", prepared)
	}
	if _, _, err := store.Load("uncertain"); err != nil {
		t.Fatalf("load after recovery: %v", err)
	}
}

// A log written before stable IDs existed still loads, and the duplicate index
// treats those events as distinct logical events.
func TestOldLogWithoutEventIDsLoadsAndAppends(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(Snapshot{SessionID: "legacy"}); err != nil {
		t.Fatal(err)
	}
	eventsPath := filepath.Join(store.Root(), "legacy", "events.jsonl")
	lines := []string{
		`{"version":2,"sequence":1,"type":"session_created","session_id":"legacy","at":"2026-01-01T00:00:00Z"}`,
		`{"version":2,"sequence":2,"type":"prompt_recorded","session_id":"legacy","at":"2026-01-01T00:00:01Z","prompt":"old"}`,
	}
	if err := os.WriteFile(eventsPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A new store has no cached index, so it reads the legacy log first.
	reopened, err := Open(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, diagnostics, err := reopened.Load("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 || len(snapshot.PromptHistory) != 1 || snapshot.PromptHistory[0] != "old" {
		t.Fatalf("snapshot=%#v diagnostics=%#v", snapshot, diagnostics)
	}
	prepared, err := reopened.Append("legacy", []Event{{Type: EventPromptRecorded, Prompt: "new"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 1 || prepared[0].Sequence != 3 || prepared[0].ID == "" {
		t.Fatalf("prepared = %#v", prepared)
	}
}

// A log that already contains one event ID twice is diagnosed instead of being
// applied twice.
func TestLoadDiagnosesRepeatedEventIDs(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(Snapshot{SessionID: "repeated"}); err != nil {
		t.Fatal(err)
	}
	eventsPath := filepath.Join(store.Root(), "repeated", "events.jsonl")
	repeated := `{"version":2,"id":"event-shared","type":"prompt_recorded","session_id":"repeated","at":"2026-01-01T00:00:00Z","prompt":"same"}`
	conflicting := `{"version":2,"id":"event-shared","type":"prompt_recorded","session_id":"repeated","at":"2026-01-01T00:00:01Z","prompt":"different"}`
	lines := []string{
		`{"version":2,"sequence":1,"type":"session_created","session_id":"repeated","at":"2025-12-31T00:00:00Z"}`,
		`{"version":2,"sequence":2,"id":"event-shared",` + strings.TrimPrefix(repeated, `{"version":2,`),
		`{"version":2,"sequence":3,` + strings.TrimPrefix(repeated, `{"version":2,`),
	}
	if err := os.WriteFile(eventsPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, diagnostics, err := store.Load("repeated")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.PromptHistory) != 1 {
		t.Fatalf("a repeated event was applied twice: %#v", snapshot.PromptHistory)
	}
	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0].Message, "repeated event ID") {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}

	lines[2] = `{"version":2,"sequence":3,` + strings.TrimPrefix(conflicting, `{"version":2,`)
	if err := os.WriteFile(eventsPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	_, diagnostics, err = reopened.Load("repeated")
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0].Message, "conflicting content") {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}

// A session document from another schema version is refused explicitly rather
// than misread.
func TestOlderSchemaVersionIsRefusedExplicitly(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(store.Root(), "old")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"version": 1, "session_id": "old"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "snapshot.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load("old"); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("err = %v", err)
	}
}
