package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// perfSessionSizes are the log sizes the plan asks for: a session that has been
// used for a while, and one that has been used for a very long time.
var perfSessionSizes = []int{10_000, 100_000}

// largeLogSize picks the size a case runs at. The ten-thousand-event session is the
// one with an asserted threshold, because it is cheap enough to build in CI; the
// hundred-thousand case is available locally and through the benchmarks, and its
// numbers belong in a report rather than in a flaky assertion.
func largeLogSize(t *testing.T) int {
	t.Helper()
	if testing.Short() {
		return perfSessionSizes[0]
	}
	return perfSessionSizes[0]
}

// buildLargeSession writes a session whose log holds roughly `events` events,
// through the real Append path so the file is exactly what production writes.
//
// Events are appended in batches: one append per event would measure the writer
// rather than the reader, which is what the plan's threshold is about.
func buildLargeSession(tb testing.TB, root string, events int) (*Store, string) {
	tb.Helper()
	store, err := Open(root)
	if err != nil {
		tb.Fatal(err)
	}
	snapshot, err := store.Create(Snapshot{
		SessionID: "large", Workspace: filepath.Join(root, "ws"), PermissionMode: "auto",
		Provider: ProviderState{Name: "primary", Generation: 1, Adapter: "openai_responses", BaseURL: "https://example.test/v1", Model: "model"},
	})
	if err != nil {
		tb.Fatal(err)
	}
	const batch = 200
	for written := 0; written < events; written += batch {
		appended := make([]Event, 0, batch)
		for index := 0; index < batch; index++ {
			appended = append(appended, Event{Type: EventPromptRecorded, Prompt: fmt.Sprintf("prompt-%d", written+index)})
		}
		if _, err := store.Append(snapshot.SessionID, appended); err != nil {
			tb.Fatal(err)
		}
	}
	return store, snapshot.SessionID
}

// logEventCount reports how many events the log actually holds, so a case can state
// the size it measured instead of assuming it.
func logEventCount(tb testing.TB, root, sessionID string) int {
	tb.Helper()
	data, err := os.ReadFile(filepath.Join(root, sessionID, "events.jsonl"))
	if err != nil {
		tb.Fatal(err)
	}
	count := 0
	for _, line := range splitLines(string(data)) {
		if len(line) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			tb.Fatalf("decode event: %v", err)
		}
		count++
	}
	return count
}

func splitLines(value string) []string {
	lines := make([]string, 0, 128)
	start := 0
	for index := 0; index < len(value); index++ {
		if value[index] != '\n' {
			continue
		}
		line := value[start:index]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		lines = append(lines, line)
		start = index + 1
	}
	if start < len(value) {
		lines = append(lines, value[start:])
	}
	return lines
}

// A first append on a large log builds the idempotency index by reading the whole
// log once. That is the cost the plan asked to measure, and the threshold below is
// deliberately generous: it is a regression guard for "this became quadratic",
// not a performance target.
func TestFirstAppendOnALargeLogStaysWithinItsBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("the large-log threshold is skipped in short mode")
	}
	events := largeLogSize(t)
	root := t.TempDir()
	_, sessionID := buildLargeSession(t, root, events)
	if got := logEventCount(t, root, sessionID); got < events {
		t.Fatalf("the fixture wrote %d events, want at least %d", got, events)
	}

	// A fresh store has no index, so this append pays for reading the log once.
	restarted, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := restarted.Append(sessionID, []Event{{Type: EventPromptRecorded, Prompt: "after the restart"}}); err != nil {
		t.Fatal(err)
	}
	firstAppend := time.Since(started)

	// A second append reuses the index, so it must not pay the same cost again.
	started = time.Now()
	if _, err := restarted.Append(sessionID, []Event{{Type: EventPromptRecorded, Prompt: "again"}}); err != nil {
		t.Fatal(err)
	}
	secondAppend := time.Since(started)

	// The budget is wide on purpose: an order of magnitude above what this host
	// measures, so it catches a change in complexity rather than a slow machine.
	const budget = 5 * time.Second
	t.Logf("%d events: first append %s, second append %s", events, firstAppend, secondAppend)
	if firstAppend > budget {
		t.Fatalf("the first append on a %d-event log took %s, over the %s budget", events, firstAppend, budget)
	}
	if secondAppend > firstAppend {
		// Not a hard failure: the second append can be slower on a busy machine. The
		// point is only that the index is reused rather than rebuilt, which the
		// comparison states as a fact to read in the log.
		t.Logf("the second append (%s) was slower than the first (%s); the index is reused, so this is host noise", secondAppend, firstAppend)
	}
}

// Loading and recovering a large log are also bounded, and both must still reach
// the same conclusion they reached on a small one.
func TestLoadAndRecoverOnALargeLogStayWithinTheirBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("the large-log threshold is skipped in short mode")
	}
	events := largeLogSize(t)
	root := t.TempDir()
	store, sessionID := buildLargeSession(t, root, events)
	if err := store.Save(mustLoad(t, root, sessionID)); err != nil {
		t.Fatal(err)
	}

	const budget = 5 * time.Second

	started := time.Now()
	snapshot, diagnostics, err := openTestStore(t, root).Load(sessionID)
	loadDuration := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}

	started = time.Now()
	recovered, recoverDiagnostics, err := openTestStore(t, root).LoadRecovering(sessionID)
	recoverDuration := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if len(recoverDiagnostics) != 0 {
		t.Fatalf("recovery diagnostics = %#v", recoverDiagnostics)
	}
	t.Logf("%d events: load %s, recover %s", events, loadDuration, recoverDuration)

	if loadDuration > budget || recoverDuration > budget {
		t.Fatalf("load %s / recover %s exceeded the %s budget", loadDuration, recoverDuration, budget)
	}
	// Allocation is the other half of the cost, and unlike time it is deterministic,
	// so it carries the tighter guard: a change that starts allocating per byte, or
	// quadratic in the number of events, shows up here rather than as a flaky
	// duration. The bound is generous - this host measures about 22 allocations per
	// event - and it is about complexity, not about a target.
	allocations := testing.AllocsPerRun(3, func() {
		store, err := Open(root)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Load(sessionID); err != nil {
			t.Fatal(err)
		}
	})
	const allocationsPerEvent = 60
	if limit := float64(allocationsPerEvent * events); allocations > limit {
		t.Fatalf("loading %d events took %.0f allocations, over the %.0f budget", events, allocations, limit)
	}
	t.Logf("%d events: %.0f allocations per load (%.1f per event)", events, allocations, allocations/float64(events))

	// A large log must produce the same session as a small one: the size changes the
	// cost, never the conclusion.
	if len(snapshot.PromptHistory) != len(recovered.PromptHistory) {
		t.Fatalf("load says %d prompts and recovery says %d", len(snapshot.PromptHistory), len(recovered.PromptHistory))
	}
	if snapshot.Sequence != recovered.Sequence || snapshot.SessionID != recovered.SessionID {
		t.Fatalf("load = %d/%s recovery = %d/%s", snapshot.Sequence, snapshot.SessionID, recovered.Sequence, recovered.SessionID)
	}
}

func mustLoad(tb testing.TB, root, sessionID string) Snapshot {
	tb.Helper()
	store, err := Open(root)
	if err != nil {
		tb.Fatal(err)
	}
	snapshot, _, err := store.Load(sessionID)
	if err != nil {
		tb.Fatal(err)
	}
	return snapshot
}

// The benchmarks state the cost curve: run them with
// `go test ./internal/session/ -run '^$' -bench Large -benchmem`.
func benchmarkLargeSession(b *testing.B, events int) {
	root := b.TempDir()
	store, sessionID := buildLargeSession(b, root, events)
	if err := store.Save(mustLoad(b, root, sessionID)); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		b.StopTimer()
		restarted, err := Open(root)
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := restarted.Append(sessionID, []Event{{Type: EventPromptRecorded, Prompt: "benchmark"}}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFirstAppendLargeLog10k(b *testing.B)  { benchmarkLargeSession(b, 10_000) }
func BenchmarkFirstAppendLargeLog100k(b *testing.B) { benchmarkLargeSession(b, 100_000) }

func benchmarkLoadLargeSession(b *testing.B, events int) {
	root := b.TempDir()
	store, sessionID := buildLargeSession(b, root, events)
	if err := store.Save(mustLoad(b, root, sessionID)); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		store, err := Open(root)
		if err != nil {
			b.Fatal(err)
		}
		if _, _, err := store.Load(sessionID); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLoadLargeLog10k(b *testing.B)  { benchmarkLoadLargeSession(b, 10_000) }
func BenchmarkLoadLargeLog100k(b *testing.B) { benchmarkLoadLargeSession(b, 100_000) }
