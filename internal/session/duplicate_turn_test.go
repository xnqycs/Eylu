package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"Eylu/internal/protocol"
)

// duplicateLastTurnWithoutID rewrites the event log so its last turn appears
// twice without an event ID, which is what a log written before stable event IDs
// looked like. The raw bytes are otherwise untouched, so a test can assert that a
// load leaves them alone.
func duplicateLastTurnWithoutID(t *testing.T, root, sessionID string, change func(*protocol.Turn)) {
	t.Helper()
	path := filepath.Join(root, sessionID, "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	last := lines[len(lines)-1]

	// Drop the ID from the original first: a pre-ID log holds neither copy's ID.
	var raw map[string]any
	if err := json.Unmarshal([]byte(last), &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "id")
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	lines[len(lines)-1] = string(encoded)

	var original Event
	if err := json.Unmarshal(encoded, &original); err != nil {
		t.Fatal(err)
	}
	if original.Type != EventTurnAppended || original.Turn == nil {
		t.Fatalf("the last event is not a turn: %#v", original)
	}
	duplicate := original
	turnCopy := *original.Turn
	if change != nil {
		change(&turnCopy)
	}
	duplicate.Turn = &turnCopy
	duplicate.ID = ""
	duplicate.Sequence = original.Sequence + 1
	duplicate.At = original.At.Add(time.Second)
	encodedDuplicate, err := json.Marshal(duplicate)
	if err != nil {
		t.Fatal(err)
	}
	lines = append(lines, string(encodedDuplicate))

	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func eventLogBytes(t *testing.T, root, sessionID string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, sessionID, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A pre-ID log that repeats a turn verbatim is merged: the decision is usable, one
// copy is applied, and the repetition is reported instead of being applied twice.
func TestLoadMergesATurnRepeatedWithIdenticalContent(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	createTestSession(t, store, "legacy", filepath.Join(root, "ws"))
	turn := textTurn("turn-1", protocol.RoleUser, "hello")
	if _, err := store.Append("legacy", []Event{{Type: EventTurnAppended, Turn: &turn}}); err != nil {
		t.Fatal(err)
	}
	duplicateLastTurnWithoutID(t, root, "legacy", nil)
	before := eventLogBytes(t, root, "legacy")

	snapshot, diagnostics, err := openTestStore(t, root).LoadRecovering("legacy")
	if err != nil {
		t.Fatalf("a repeated turn made the session unreadable: %v", err)
	}
	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0].Message, "merged turn") {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	if !diagnostics[0].Benign {
		t.Fatalf("a merge that lost nothing was reported as needing a human: %#v", diagnostics[0])
	}
	if len(snapshot.Turns) != 1 || snapshot.Turns[0].ID != "turn-1" {
		t.Fatalf("turns = %#v", snapshot.Turns)
	}
	if after := eventLogBytes(t, root, "legacy"); after != before {
		t.Fatal("the load rewrote the event log")
	}
}

// A repeated turn ID whose content differs is reported and left alone: the first
// copy is kept, the log keeps both, and the diagnostic says a human has to look.
func TestLoadReportsATurnRepeatedWithDifferentContent(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	createTestSession(t, store, "conflict", filepath.Join(root, "ws"))
	turn := textTurn("turn-1", protocol.RoleUser, "hello")
	if _, err := store.Append("conflict", []Event{{Type: EventTurnAppended, Turn: &turn}}); err != nil {
		t.Fatal(err)
	}
	duplicateLastTurnWithoutID(t, root, "conflict", func(turn *protocol.Turn) {
		turn.Parts = []protocol.Part{{Kind: protocol.PartText, Text: "something else"}}
	})
	before := eventLogBytes(t, root, "conflict")
	beforeLines := strings.Count(before, "\n")

	snapshot, diagnostics, err := openTestStore(t, root).LoadRecovering("conflict")
	if err != nil {
		t.Fatalf("a conflicting turn made the session unreadable: %v", err)
	}
	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0].Message, "different content") {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	if diagnostics[0].Benign {
		t.Fatal("a conflicting turn was reported as resolved")
	}
	if len(snapshot.Turns) != 1 || snapshot.Turns[0].Parts[0].Text != "hello" {
		t.Fatalf("the first copy was not the one kept: %#v", snapshot.Turns)
	}
	after := eventLogBytes(t, root, "conflict")
	if after != before || strings.Count(after, "\n") != beforeLines {
		t.Fatal("the load rewrote the conflicting log instead of reporting it")
	}
}

// A repeated event ID whose content is identical is the same retry the store
// already handles; it is now also reported as resolved rather than as something a
// human has to look at, so it no longer blocks a resume.
func TestRepeatedIdenticalEventIDIsReportedAsResolved(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	createTestSession(t, store, "retry", filepath.Join(root, "ws"))
	// A prompt event is used rather than a turn, because a repeated turn ID is
	// reported by the more specific turn diagnostic that runs first.
	appended, err := store.Append("retry", []Event{{Type: EventPromptRecorded, Prompt: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	// Write the same event again with the same ID, exactly as an uncertain append
	// that succeeded would leave it.
	path := filepath.Join(root, "retry", "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := appended[0]
	// A retried append is written under the next sequence but keeps its event ID,
	// which is exactly the duplicate the store has to recognize.
	duplicate.Sequence = appended[0].Sequence + 1
	encoded, err := json.Marshal(duplicate)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, append(encoded, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}

	_, diagnostics, err := openTestStore(t, root).LoadRecovering("retry")
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0].Message, "repeated event ID") {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	if !diagnostics[0].Benign {
		t.Fatalf("an identical retry was reported as needing a human: %#v", diagnostics[0])
	}
}
