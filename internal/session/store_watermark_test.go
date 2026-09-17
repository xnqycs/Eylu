package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"Eylu/internal/protocol"
)

// writeLogLine appends one hand-written record to a session log, which is how a
// log that already holds a repeated record is reproduced.
func writeLogLine(t *testing.T, store *Store, id, line string) {
	t.Helper()
	path := filepath.Join(store.Root(), id, "events.jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// logSequences returns the physical sequence of every record in a log, in the
// order the file holds them.
func logSequences(t *testing.T, store *Store, id string) []uint64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(store.Root(), id, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sequences := make([]uint64, 0)
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("undecodable log line %q: %v", line, err)
		}
		sequences = append(sequences, event.Sequence)
	}
	return sequences
}

// assertContiguousLog requires every physical record to follow the previous one by
// exactly one, which is what the reader enforces on the next load.
func assertContiguousLog(t *testing.T, store *Store, id string) {
	t.Helper()
	sequences := logSequences(t, store, id)
	seen := make(map[uint64]struct{}, len(sequences))
	for index, sequence := range sequences {
		if sequence != uint64(index)+1 {
			t.Fatalf("log sequence at position %d is %d, want %d (all: %v)", index, sequence, index+1, sequences)
		}
		if _, duplicate := seen[sequence]; duplicate {
			t.Fatalf("log sequence %d appears twice: %v", sequence, sequences)
		}
		seen[sequence] = struct{}{}
	}
}

func logLine(t *testing.T, event Event) string {
	t.Helper()
	event.Version = SchemaVersion
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// resetLog restores a session log to exactly the bytes Create wrote, so a test can
// build one hand-written scenario after another.
func resetLog(t *testing.T, store *Store, id string, created []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(store.Root(), id, "events.jsonl"), created, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readLogBytes(t *testing.T, store *Store, id string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(store.Root(), id, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A repeated record in the middle of a log is consumed without being applied a
// second time, and the records behind it still load.
//
// The physical sequence is the log's own watermark, so deduplicating the content
// must not pretend the repeated line is not there: treating it as a gap makes a
// session with one repeated record unreadable even though nothing is missing.
func TestLoadConsumesARepeatedRecordBeforeLaterRecords(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	createTestSession(t, store, "log", filepath.Join(root, "ws"))
	created := readLogBytes(t, store, "log")
	repeated := logLine(t, Event{Sequence: 2, ID: "event-shared", Type: EventPromptRecorded, SessionID: "log", Prompt: "first"})
	later := logLine(t, Event{Sequence: 4, ID: "event-later", Type: EventPromptRecorded, SessionID: "log", Prompt: "later"})

	tests := []struct {
		name   string
		repeat string
		benign bool
	}{
		{
			name:   "identical repeat",
			repeat: logLine(t, Event{Sequence: 3, ID: "event-shared", Type: EventPromptRecorded, SessionID: "log", Prompt: "first"}),
			benign: true,
		},
		{
			name:   "conflicting repeat",
			repeat: logLine(t, Event{Sequence: 3, ID: "event-shared", Type: EventPromptRecorded, SessionID: "log", Prompt: "different"}),
			benign: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resetLog(t, store, "log", created)
			writeLogLine(t, store, "log", repeated)
			writeLogLine(t, store, "log", test.repeat)
			writeLogLine(t, store, "log", later)

			snapshot, diagnostics, err := openTestStore(t, root).LoadRecovering("log")
			if err != nil {
				t.Fatalf("a repeated record made the session unreadable: %v", err)
			}
			want := []string{"first", "later"}
			if len(snapshot.PromptHistory) != len(want) {
				t.Fatalf("prompts = %#v, want %v", snapshot.PromptHistory, want)
			}
			for index, value := range want {
				if snapshot.PromptHistory[index] != value {
					t.Fatalf("prompts = %#v, want %v", snapshot.PromptHistory, want)
				}
			}
			if len(diagnostics) != 1 {
				t.Fatalf("diagnostics = %#v, want exactly one report of the repeat", diagnostics)
			}
			if diagnostics[0].Benign != test.benign {
				t.Fatalf("the repeat was classified wrong: %#v", diagnostics[0])
			}
			// The repeated physical record is consumed by the watermark, so a
			// snapshot written from this load already covers the whole log.
			if snapshot.Sequence != 4 {
				t.Fatalf("snapshot watermark = %d, want the last physical record 4", snapshot.Sequence)
			}
		})
	}
}

// Appending after a load continues from the last physical record, not from the
// last applied one, so a log that repeats a record at its end stays writable.
func TestAppendContinuesFromThePhysicalTailAfterARepeatedRecord(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	createTestSession(t, store, "log", filepath.Join(root, "ws"))
	writeLogLine(t, store, "log", logLine(t, Event{Sequence: 2, ID: "event-shared", Type: EventPromptRecorded, SessionID: "log", Prompt: "first"}))
	writeLogLine(t, store, "log", logLine(t, Event{Sequence: 3, ID: "event-shared", Type: EventPromptRecorded, SessionID: "log", Prompt: "first"}))

	reopened := openTestStore(t, root)
	snapshot, _, err := reopened.LoadRecovering("log")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(snapshot.PromptHistory) != 1 || snapshot.PromptHistory[0] != "first" {
		t.Fatalf("prompts = %#v", snapshot.PromptHistory)
	}
	prepared, err := reopened.Append("log", []Event{{ID: "event-after", Type: EventPromptRecorded, Prompt: "after"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 1 || prepared[0].Sequence != 4 {
		t.Fatalf("the append reused a physical sequence: %#v", prepared)
	}
	assertContiguousLog(t, reopened, "log")
	// The log has to stay loadable: a reused sequence reads back as a gap.
	reloaded, _, err := openTestStore(t, root).LoadRecovering("log")
	if err != nil {
		t.Fatalf("the log became unreadable after an append: %v", err)
	}
	if len(reloaded.PromptHistory) != 2 {
		t.Fatalf("prompts = %#v", reloaded.PromptHistory)
	}
}

// Loading, appending and saving around a repeated record keeps working for as
// many rounds as the session is used.
func TestLoadAppendSaveCyclesKeepTheLogContiguous(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	createTestSession(t, store, "log", filepath.Join(root, "ws"))
	// A retry of one logical prompt reached the log twice, which is what an
	// uncertain append that actually succeeded leaves behind.
	writeLogLine(t, store, "log", logLine(t, Event{Sequence: 2, ID: "event-shared", Type: EventPromptRecorded, SessionID: "log", Prompt: "first"}))
	writeLogLine(t, store, "log", logLine(t, Event{Sequence: 3, ID: "event-shared", Type: EventPromptRecorded, SessionID: "log", Prompt: "first"}))

	for round := 0; round < 3; round++ {
		reopened := openTestStore(t, root)
		snapshot, _, err := reopened.LoadRecovering("log")
		if err != nil {
			t.Fatalf("round %d: load: %v", round, err)
		}
		prepared, err := reopened.Append("log", []Event{{Type: EventPromptRecorded, Prompt: "round"}})
		if err != nil {
			t.Fatalf("round %d: append: %v", round, err)
		}
		snapshot.Sequence = prepared[0].Sequence
		snapshot.PromptHistory = append(snapshot.PromptHistory, "round")
		if err := reopened.Save(snapshot); err != nil {
			t.Fatalf("round %d: save: %v", round, err)
		}
		assertContiguousLog(t, reopened, "log")
	}

	final := openTestStore(t, root)
	snapshot, _, err := final.LoadRecovering("log")
	if err != nil {
		t.Fatalf("final load: %v", err)
	}
	want := []string{"first", "round", "round", "round"}
	if len(snapshot.PromptHistory) != len(want) {
		t.Fatalf("prompts = %#v, want %v", snapshot.PromptHistory, want)
	}
	for index, value := range want {
		if snapshot.PromptHistory[index] != value {
			t.Fatalf("prompts = %#v, want %v", snapshot.PromptHistory, want)
		}
	}
}

// A damaged tail is repaired, and the sequence the next append continues from is
// the last valid physical record - with a repeated record earlier in the same log
// consumed by the watermark rather than treated as a gap.
func TestDamagedTailRepairKeepsTheSequenceAfterARepeatedRecord(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	createTestSession(t, store, "log", filepath.Join(root, "ws"))
	writeLogLine(t, store, "log", logLine(t, Event{Sequence: 2, ID: "event-shared", Type: EventPromptRecorded, SessionID: "log", Prompt: "first"}))
	writeLogLine(t, store, "log", logLine(t, Event{Sequence: 3, ID: "event-shared", Type: EventPromptRecorded, SessionID: "log", Prompt: "first"}))
	writeLogLine(t, store, "log", logLine(t, Event{Sequence: 4, ID: "event-later", Type: EventPromptRecorded, SessionID: "log", Prompt: "later"}))
	// A half-written record, which is what a crash in the middle of an append
	// leaves behind.
	path := filepath.Join(store.Root(), "log", "events.jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"version":3,"sequence":5,"type":"prompt_rec`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openTestStore(t, root)
	snapshot, diagnostics, err := reopened.LoadRecovering("log")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(snapshot.PromptHistory) != 2 {
		t.Fatalf("prompts = %#v, diagnostics = %#v", snapshot.PromptHistory, diagnostics)
	}
	if len(logSequences(t, reopened, "log")) != 4 {
		t.Fatalf("the damaged tail was not repaired: %v", logSequences(t, reopened, "log"))
	}
	prepared, err := reopened.Append("log", []Event{{Type: EventPromptRecorded, Prompt: "after"}})
	if err != nil {
		t.Fatal(err)
	}
	if prepared[0].Sequence != 5 {
		t.Fatalf("append continued at %d, want the next free sequence 5", prepared[0].Sequence)
	}
	assertContiguousLog(t, reopened, "log")
	if _, _, err := openTestStore(t, root).LoadRecovering("log"); err != nil {
		t.Fatalf("the repaired log became unreadable: %v", err)
	}
}

// A repeated turn under a different event ID sits in the middle of the log just
// as a repeated event ID does, and the records behind it must still load.
func TestLoadConsumesARepeatedTurnBeforeLaterRecords(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	createTestSession(t, store, "turns", filepath.Join(root, "ws"))
	first := textTurn("turn-1", protocol.RoleUser, "hello")
	second := textTurn("turn-2", protocol.RoleUser, "later")
	writeLogLine(t, store, "turns", logLine(t, Event{Sequence: 2, ID: "event-a", Type: EventTurnAppended, SessionID: "turns", Turn: &first}))
	// The same turn again under another event ID, with the same content.
	writeLogLine(t, store, "turns", logLine(t, Event{Sequence: 3, ID: "event-b", Type: EventTurnAppended, SessionID: "turns", Turn: &first}))
	writeLogLine(t, store, "turns", logLine(t, Event{Sequence: 4, ID: "event-c", Type: EventTurnAppended, SessionID: "turns", Turn: &second}))

	snapshot, diagnostics, err := openTestStore(t, root).LoadRecovering("turns")
	if err != nil {
		t.Fatalf("a repeated turn in the middle made the session unreadable: %v", err)
	}
	if len(snapshot.Turns) != 2 || snapshot.Turns[0].ID != "turn-1" || snapshot.Turns[1].ID != "turn-2" {
		t.Fatalf("turns = %#v", snapshot.Turns)
	}
	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0].Message, "merged turn") {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}
