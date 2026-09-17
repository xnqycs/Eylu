package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

// soakRounds bounds how long the soak cases run. They are short by default so they
// are useful in CI; EYLU_SOAK_ROUNDS lengthens them for a local run that is meant to
// find something rather than to regression-test.
func soakRounds(t *testing.T) int {
	t.Helper()
	rounds := 6
	if raw := strings.TrimSpace(os.Getenv("EYLU_SOAK_ROUNDS")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			t.Fatalf("EYLU_SOAK_ROUNDS=%q is not a positive number", raw)
		}
		rounds = parsed
	}
	return rounds
}

// soakExecutor builds an executor that really coordinates resources and really
// writes files, which is what makes the assertions below about the production path.
func soakExecutor(t *testing.T, workspace string, parallel int) *Executor {
	t.Helper()
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	return &Executor{
		Registry: NewRegistry(write), Policy: policy.AllowAllChecker{},
		Workspace: workspace, Coordinator: NewResourceCoordinator(), MaxParallelTools: parallel,
	}
}

// coordinatorDrained reports the waiters and grants an executor still holds.
func coordinatorDrained(executor *Executor) (waiters, active int) {
	if executor.Coordinator == nil {
		return 0, 0
	}
	executor.Coordinator.mu.Lock()
	defer executor.Coordinator.mu.Unlock()
	return len(executor.Coordinator.waiters), len(executor.Coordinator.active)
}

// goroutineGrowth runs fn and reports how many goroutines the process gained, after
// giving the runtime a bounded window to settle. It is a leak check, not a precise
// count: a case that starts and finishes work must not leave a goroutine behind.
func goroutineGrowth(t *testing.T, fn func()) int {
	t.Helper()
	baseline := runtime.NumGoroutine()
	fn()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if runtime.NumGoroutine() <= baseline {
			return 0
		}
		time.Sleep(20 * time.Millisecond)
	}
	return runtime.NumGoroutine() - baseline
}

// Conflicting and independent claims run side by side for many rounds, and the
// invariants that matter hold at the end of every one: every call reached a
// terminal state, the coordinator holds nothing, and a file that several calls
// wrote holds exactly one of the written contents rather than a mixture.
func TestSoakParallelBatchesOnConflictingResources(t *testing.T) {
	rounds := soakRounds(t)
	workspace := t.TempDir()
	executor := soakExecutor(t, workspace, 4)
	shared := filepath.Join(workspace, "shared.txt")

	witness := newOverlapWitness(2 * time.Millisecond)
	sink := newRecordingSink()
	executor.Registry = NewRegistry(witness.wrap(t, workspace))
	executor.Checkpoint = sink

	growth := goroutineGrowth(t, func() {
		for round := 0; round < rounds; round++ {
			calls := make([]protocol.ToolCall, 0, 8)
			for index := 0; index < 4; index++ {
				// Two writers of the same path conflict; the rest are independent.
				path := shared
				if index >= 2 {
					path = filepath.Join(workspace, fmt.Sprintf("round-%d-%d.txt", round, index))
				}
				calls = append(calls, writeCall(fmt.Sprintf("r%d-%d", round, index), path, fmt.Sprintf("round-%d-call-%d", round, index)))
			}
			// The fixture must exercise claim-based concurrency. Without this the
			// overlap assertion below could be measuring exclusive scheduling, which
			// would make it look meaningful while testing something else.
			for _, call := range calls {
				item, ok := executor.Registry.Get(call.Name)
				if !ok {
					t.Fatalf("round %d: tool %q is not registered", round, call.Name)
				}
				classifier, ok := item.(ConcurrencyClassifier)
				if !ok {
					t.Fatalf("round %d: tool %q does not classify its concurrency", round, call.Name)
				}
				spec := normalizeConcurrencySpec(classifier.ClassifyConcurrency(call.Arguments, policy.Outcome{Risk: item.Risk()}))
				if spec.Mode != ConcurrencyClaimed {
					t.Fatalf("round %d: call %s on %s classified as %q; this fixture does not test conflicting claims", round, call.ID, call.Arguments, spec.Mode)
				}
			}
			results, outcome := executor.ExecuteBatchOutcome(ctx(), fmt.Sprintf("request-%d", round), calls, BatchHooks{}, 0)
			if outcome.Control != protocol.ControlContinue {
				t.Fatalf("round %d control = %q cause = %v", round, outcome.Control, outcome.Cause)
			}
			if len(results) != len(calls) {
				t.Fatalf("round %d returned %d results for %d calls", round, len(results), len(calls))
			}
			for index, result := range results {
				if result.State == "" {
					t.Fatalf("round %d call %d has no terminal state: %#v", round, index, result)
				}
				if result.IsError {
					t.Fatalf("round %d call %d failed: %s", round, index, result.Content)
				}
			}
			if waiters, active := coordinatorDrained(executor); waiters != 0 || active != 0 {
				t.Fatalf("round %d left the coordinator busy: waiters=%d active=%d", round, waiters, active)
			}
			// The two calls that claim one path must not have overlapped. The file
			// itself cannot show this - write_file replaces atomically, so a torn
			// value is impossible either way - which is why the overlap is observed
			// inside the execution instead.
			//
			// The guard is proven to fire: disabling the scheduler's `canStartCall`
			// together with the coordinator's conflict predicate makes it report two
			// calls running on one path at once. It took a second attempt to get
			// there, and the reason is worth keeping: the first version of the
			// witness forwarded only Definition and Risk to the real tool, which
			// stripped the ConcurrencyClassifier interface, so the executor
			// classified every call fail-closed as exclusive and the two writers
			// could never overlap no matter which guard was removed. The wrapper now
			// embeds the real tool so it stays interface-transparent, and the loop
			// above asserts the fixture really classifies its calls as claimed
			// rather than taking that for granted.
			if overlapped := witness.overlaps(); len(overlapped) > 0 {
				t.Fatalf("round %d ran two calls on the same path at once: %v", round, overlapped)
			}
			// Every call reached the log exactly once, so no side effect was
			// repeated and none was left unrecorded.
			if repeats := sink.repeated(); len(repeats) > 0 {
				t.Fatalf("round %d recorded the same call twice: %v", round, repeats)
			}
			if missing := sink.incomplete(calls); len(missing) > 0 {
				t.Fatalf("round %d left calls without a completion: %v", round, missing)
			}
		}
	})
	if witness.executions() == 0 {
		t.Fatal("the witness never observed an execution, so the overlap check measured nothing")
	}
	if growth > 0 {
		t.Fatalf("the parallel rounds leaked %d goroutine(s)", growth)
	}
}

// Cancelling batches over and over must leave nothing behind: no waiter, no open
// call, and no goroutine. This is the combination a request that is cancelled,
// tightened and restarted produces.
func TestSoakRepeatedCancellationLeavesNothingBehind(t *testing.T) {
	rounds := soakRounds(t)
	workspace := t.TempDir()
	executor := soakExecutor(t, workspace, 2)

	growth := goroutineGrowth(t, func() {
		for round := 0; round < rounds; round++ {
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			calls := []protocol.ToolCall{
				writeCall(fmt.Sprintf("c%d-a", round), filepath.Join(workspace, "a.txt"), "a"),
				writeCall(fmt.Sprintf("c%d-b", round), filepath.Join(workspace, "b.txt"), "b"),
			}
			results, outcome := executor.ExecuteBatchOutcome(cancelled, fmt.Sprintf("cancelled-%d", round), calls, BatchHooks{}, 0)
			if outcome.Control == protocol.ControlContinue {
				t.Fatalf("round %d did not report the cancellation", round)
			}
			for index, result := range results {
				if result.State == "" {
					t.Fatalf("round %d call %d has no terminal state after cancellation", round, index)
				}
				if result.State == protocol.CallSucceeded {
					t.Fatalf("round %d call %d claimed success although it was cancelled", round, index)
				}
			}
			if waiters, active := coordinatorDrained(executor); waiters != 0 || active != 0 {
				t.Fatalf("round %d left the coordinator busy: waiters=%d active=%d", round, waiters, active)
			}
		}
	})
	if growth > 0 {
		t.Fatalf("repeated cancellation leaked %d goroutine(s)", growth)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(workspace, name)); !os.IsNotExist(err) {
			t.Fatalf("%s was written although every batch was cancelled", name)
		}
	}
}

// Audit and checkpoint faults injected into every round must not change what the
// batch reports or leave the executor holding resources.
func TestSoakHostCallbackFaultsDoNotRotTheOutcome(t *testing.T) {
	rounds := soakRounds(t)
	workspace := t.TempDir()
	executor := soakExecutor(t, workspace, 4)
	audit := &faultPanicAudit{}
	executor.Audit = audit
	executor.Checkpoint = &flakySink{failEvery: 3}

	growth := goroutineGrowth(t, func() {
		for round := 0; round < rounds; round++ {
			calls := []protocol.ToolCall{
				writeCall(fmt.Sprintf("f%d-a", round), filepath.Join(workspace, "one.txt"), "one"),
				writeCall(fmt.Sprintf("f%d-b", round), filepath.Join(workspace, "two.txt"), "two"),
			}
			results, _ := executor.ExecuteBatchOutcome(ctx(), fmt.Sprintf("faults-%d", round), calls, BatchHooks{}, 0)
			if len(results) != len(calls) {
				t.Fatalf("round %d returned %d results", round, len(results))
			}
			for index, result := range results {
				if result.State == "" {
					t.Fatalf("round %d call %d has no terminal state although a host callback failed", round, index)
				}
			}
			if waiters, active := coordinatorDrained(executor); waiters != 0 || active != 0 {
				t.Fatalf("round %d left the coordinator busy: waiters=%d active=%d", round, waiters, active)
			}
		}
	})
	if growth > 0 {
		t.Fatalf("host callback faults leaked %d goroutine(s)", growth)
	}
	// The audit sink panicked on every call, so every call must be counted as an
	// undelivered record rather than the panic escaping the executor.
	if audit.calls() == 0 || executor.AuditFailures() != audit.calls() {
		t.Fatalf("audit records = %d failures = %d", audit.calls(), executor.AuditFailures())
	}
}

func ctx() context.Context { return context.Background() }

func writeCall(id, path, content string) protocol.ToolCall {
	arguments, _ := json.Marshal(map[string]string{"path": path, "content": content, "reason": "soak"})
	return protocol.ToolCall{ID: id, Name: "write_file", Arguments: arguments}
}

// overlapWitness wraps the real write tool and records whether two executions ever
// ran on the same path at the same time. The tool itself cannot show that, because
// it replaces files atomically; the witness is the only place the serialization the
// coordinator promises becomes observable.
type overlapWitness struct {
	mu      sync.Mutex
	inFlt   map[string]int
	clashes map[string]int
	// hold widens the critical section so that a serialization bug is observable
	// rather than a matter of timing. It is not a guess at when something happens:
	// it is deliberately holding the window open so the overlap either happens or
	// provably cannot.
	hold time.Duration
	// runs counts the executions the witness observed, so a case can prove its own
	// fixture was exercised instead of silently measuring nothing.
	runs int
}

func newOverlapWitness(hold time.Duration) *overlapWitness {
	return &overlapWitness{inFlt: make(map[string]int), clashes: make(map[string]int), hold: hold}
}

func (w *overlapWitness) executions() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.runs
}

func (w *overlapWitness) overlaps() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	result := make([]string, 0, len(w.clashes))
	for path, count := range w.clashes {
		result = append(result, fmt.Sprintf("%s x%d", path, count))
	}
	return result
}

// wrappedWrite is the production write tool with the overlap observation around it.
//
// The write tool is embedded rather than forwarded field by field: forwarding only
// Definition and Risk silently strips the optional interfaces the executor reads -
// ConcurrencyClassifier and IntentReporter - and the executor then classifies every
// call fail-closed as exclusive. That is a test double changing production
// behaviour, and it is exactly why this wrapper is written with embedding: the
// wrapped tool stays indistinguishable from the real one except for Execute.
type wrappedWrite struct {
	*WriteFile
	witness *overlapWitness
}

func (w *overlapWitness) wrap(t *testing.T, workspace string) *wrappedWrite {
	t.Helper()
	inner, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	return &wrappedWrite{WriteFile: inner, witness: w}
}

func (w *wrappedWrite) Execute(ctx context.Context, input json.RawMessage) protocol.ToolResult {
	var fields struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(input, &fields)
	w.witness.mu.Lock()
	w.witness.runs++
	w.witness.inFlt[fields.Path]++
	if w.witness.inFlt[fields.Path] > 1 {
		w.witness.clashes[fields.Path]++
	}
	w.witness.mu.Unlock()
	defer func() {
		w.witness.mu.Lock()
		w.witness.inFlt[fields.Path]--
		w.witness.mu.Unlock()
	}()
	if w.witness.hold > 0 {
		time.Sleep(w.witness.hold)
	}
	return w.WriteFile.Execute(ctx, input)
}

// recordingSink keeps what the executor recorded, so "exactly once" can be checked
// per call rather than inferred from the files.
type recordingSink struct {
	mu          sync.Mutex
	intents     map[string]int
	completions map[string]int
}

func newRecordingSink() *recordingSink {
	return &recordingSink{intents: make(map[string]int), completions: make(map[string]int)}
}

func (s *recordingSink) RecordIntent(intent Intent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intents[intent.CallID]++
	return nil
}

func (s *recordingSink) RecordCompletion(completion Completion) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completions[completion.CallID]++
	return nil
}

func (s *recordingSink) repeated() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]string, 0)
	for callID, count := range s.intents {
		if count > 1 {
			result = append(result, fmt.Sprintf("%s x%d", callID, count))
		}
	}
	for callID, count := range s.completions {
		if count > 1 {
			result = append(result, fmt.Sprintf("%s completed x%d", callID, count))
		}
	}
	return result
}

func (s *recordingSink) incomplete(calls []protocol.ToolCall) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]string, 0)
	for _, call := range calls {
		if s.intents[call.ID] == 0 || s.completions[call.ID] == 0 {
			result = append(result, call.ID)
		}
	}
	return result
}

// faultPanicAudit panics on every record, which is the worst thing a host audit
// sink can do to a batch.
type faultPanicAudit struct {
	mu    sync.Mutex
	total int
}

func (a *faultPanicAudit) Record(AuditRecord) {
	a.mu.Lock()
	a.total++
	a.mu.Unlock()
	panic("soak: audit sink unavailable")
}

func (a *faultPanicAudit) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.total
}

// flakySink fails every nth intent, so the batch has to stop starting side effects
// on its own terms while the rounds keep coming.
type flakySink struct {
	mu        sync.Mutex
	calls     int
	failEvery int
}

func (s *flakySink) RecordIntent(Intent) error {
	s.mu.Lock()
	s.calls++
	// The count is read here and used from here: reading s.calls again after the
	// unlock raced with the next intent, which the race detector found in CI. The
	// executor calls this from a goroutine per call, so anything read outside the
	// lock is shared state.
	call := s.calls
	fail := s.failEvery > 0 && call%s.failEvery == 0
	s.mu.Unlock()
	if fail {
		return fmt.Errorf("soak: log unavailable on intent %d", call)
	}
	return nil
}

func (s *flakySink) RecordCompletion(Completion) error { return nil }

// The leak detector works: a case that starts a goroutine and never lets it finish
// is reported as growth, so a pass in the cases above means something rather than
// nothing.
func TestSoakLeakDetectorNoticesAnUnfinishedGoroutine(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	growth := goroutineGrowth(t, func() {
		go func() { <-release }()
		// Let the goroutine start before the measurement window closes.
		time.Sleep(20 * time.Millisecond)
	})
	if growth == 0 {
		t.Fatal("the leak detector did not notice a goroutine that never finished")
	}
}
