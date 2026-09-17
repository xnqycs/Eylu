package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

// faultSink records lifecycle calls and can fail either side.
//
// Like the real sink it is called from the goroutine of the call it describes, so
// its state is guarded: a test double that raced would report a race in the executor
// rather than testing it.
type faultSink struct {
	mu          sync.Mutex
	intents     []Intent
	completions []Completion
	intentErr   error
	resultErr   error
}

func (s *faultSink) RecordIntent(intent Intent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.intentErr != nil {
		return s.intentErr
	}
	s.intents = append(s.intents, intent)
	return nil
}

func (s *faultSink) RecordCompletion(completion Completion) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resultErr != nil {
		return s.resultErr
	}
	s.completions = append(s.completions, completion)
	return nil
}

// An intent that cannot be recorded prevents the operation from happening at all.
func TestCheckpointIntentFailurePreventsTheSideEffect(t *testing.T) {
	workspace := t.TempDir()
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "target.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	sink := &faultSink{intentErr: errors.New("log unavailable")}
	executor := &Executor{Registry: NewRegistry(write), Policy: policy.AllowAllChecker{}, Checkpoint: sink}
	results, outcome := executor.ExecuteBatchOutcome(context.Background(), "request", []protocol.ToolCall{{
		ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"path":"target.txt","content":"replaced","reason":"test"}`),
	}}, BatchHooks{}, 0)
	if outcome.Control != protocol.ControlAbortRequest || outcome.Cause == nil {
		t.Fatalf("outcome = %#v", outcome)
	}
	var checkpointErr *CheckpointError
	if !errors.As(outcome.Cause, &checkpointErr) || checkpointErr.Recorded {
		t.Fatalf("cause = %v", outcome.Cause)
	}
	if len(results) != 1 || results[0].State != protocol.CallNotExecuted || !strings.Contains(results[0].Content, "intent could not be recorded") {
		t.Fatalf("results = %#v", results)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "original" {
		t.Fatalf("the file was changed although the intent failed: %q", data)
	}
	if len(sink.completions) != 0 {
		t.Fatalf("a completion was recorded without an intent: %#v", sink.completions)
	}
}

// A completion that cannot be recorded keeps the result, stops the batch from
// starting new side effects, and says the operation may have happened.
func TestCheckpointCompletionFailureKeepsTheResultAndStopsTheBatch(t *testing.T) {
	workspace := t.TempDir()
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	sink := &faultSink{resultErr: errors.New("log unavailable")}
	audit := &memoryAudit{}
	executor := &Executor{
		Registry: NewRegistry(write), Policy: policy.AllowAllChecker{}, Checkpoint: sink,
		Coordinator: NewResourceCoordinator(), MaxParallelTools: 1, Audit: audit,
	}
	results, outcome := executor.ExecuteBatchOutcome(context.Background(), "request", []protocol.ToolCall{
		{ID: "first", Name: "write_file", Arguments: json.RawMessage(`{"path":"one.txt","content":"one","reason":"test"}`)},
		{ID: "second", Name: "write_file", Arguments: json.RawMessage(`{"path":"two.txt","content":"two","reason":"test"}`)},
	}, BatchHooks{}, 0)
	var checkpointErr *CheckpointError
	if !errors.As(outcome.Cause, &checkpointErr) || !checkpointErr.Recorded {
		t.Fatalf("cause = %v", outcome.Cause)
	}
	if outcome.Control != protocol.ControlAbortRequest {
		t.Fatalf("outcome = %#v", outcome)
	}
	// The first call ran and its result is kept; the second never starts.
	if len(results) != 2 || results[0].IsError || results[0].State != protocol.CallSucceeded {
		t.Fatalf("results = %#v", results)
	}
	if results[0].Metadata["checkpoint_incomplete"] != true {
		t.Fatalf("the incomplete record was not reported: %#v", results[0].Metadata)
	}
	if results[1].State != protocol.CallNotExecuted {
		t.Fatalf("second result = %#v", results[1])
	}
	if _, err := os.Stat(filepath.Join(workspace, "two.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a new side effect started after the record failure")
	}
	if len(sink.intents) != 1 || sink.intents[0].Tool != "write_file" {
		t.Fatalf("intents = %#v", sink.intents)
	}
}

// An orderedSink records both lifecycle sides in call order and keeps the two
// intent paths apart, so a test can tell a batch write from a per-call one.
//
// A batch runs its calls concurrently, so the sink has to be a concurrent data
// structure like the real one: a test double that raced would surface a race in the
// executor under `-race` instead of testing it. A test reads the fields only after
// ExecuteBatchOutcome has returned, which the completion channel orders after every
// call.
type orderedSink struct {
	mu            sync.Mutex
	intents       []Intent
	completions   []Completion
	batchWrites   int
	perCallWrites int
}

func (s *orderedSink) RecordIntent(intent Intent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.perCallWrites++
	s.intents = append(s.intents, intent)
	return nil
}

func (s *orderedSink) RecordIntents(intents []Intent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchWrites++
	s.intents = append(s.intents, intents...)
	return nil
}

func (s *orderedSink) RecordCompletion(completion Completion) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completions = append(s.completions, completion)
	return nil
}

// Describing the intent of a write must not create anything.
//
// The intent exists to make "no side effect before a durable record" true, so it
// cannot be the thing that creates the parent directories of the side effect it
// is describing: a failed intent would then leave a directory behind and
// "not executed" would be only half true.
func TestCheckpointIntentFailureLeavesNoDirectoryBehind(t *testing.T) {
	workspace := t.TempDir()
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	sink := &faultSink{intentErr: errors.New("log unavailable")}
	executor := &Executor{Registry: NewRegistry(write), Policy: policy.AllowAllChecker{}, Checkpoint: sink}
	results, outcome := executor.ExecuteBatchOutcome(context.Background(), "request", []protocol.ToolCall{{
		ID: "write", Name: "write_file",
		Arguments: json.RawMessage(`{"path":"new/parents/deep/target.txt","content":"content","create_parent_dirs":true,"reason":"test"}`),
	}}, BatchHooks{}, 0)
	var checkpointErr *CheckpointError
	if !errors.As(outcome.Cause, &checkpointErr) || checkpointErr.Recorded {
		t.Fatalf("cause = %v", outcome.Cause)
	}
	if len(results) != 1 || results[0].State != protocol.CallNotExecuted {
		t.Fatalf("results = %#v", results)
	}
	if _, err := os.Stat(filepath.Join(workspace, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a directory was created while describing an intent that never became durable")
	}
}

// The evidence a call records must describe the state that call actually saw.
//
// Two writes to one target are serialized, so the second one cannot reuse the
// evidence collected before the first one ran: a batch write that covers both
// would describe the state of the file before the first write, which is not the
// state the second call observes.
func TestCheckpointEvidenceFollowsTheStateEachCallObserved(t *testing.T) {
	workspace := t.TempDir()
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	sink := &orderedSink{}
	executor := &Executor{
		Registry: NewRegistry(write), Policy: policy.AllowAllChecker{}, Checkpoint: sink,
		Coordinator: NewResourceCoordinator(), MaxParallelTools: 1,
	}
	results, outcome := executor.ExecuteBatchOutcome(context.Background(), "request", []protocol.ToolCall{
		{ID: "first", Name: "write_file", Arguments: json.RawMessage(`{"path":"same.txt","content":"first","reason":"test"}`)},
		{ID: "second", Name: "write_file", Arguments: json.RawMessage(`{"path":"same.txt","content":"second","reason":"test"}`)},
	}, BatchHooks{}, 1)
	if outcome.Control != protocol.ControlContinue || outcome.Cause != nil {
		t.Fatalf("outcome = %#v", outcome)
	}
	for index := range results {
		if results[index].IsError || results[index].State != protocol.CallSucceeded {
			t.Fatalf("results[%d] = %#v", index, results[index])
		}
	}
	if len(sink.intents) != 2 || len(sink.completions) != 2 {
		t.Fatalf("intents = %#v completions = %#v", sink.intents, sink.completions)
	}
	if sink.intents[0].PreviousHash != "" {
		t.Fatalf("the first intent claims the file existed before: %q", sink.intents[0].PreviousHash)
	}
	if sink.intents[1].PreviousHash != sink.completions[0].ResultHash {
		t.Fatalf("the second intent recorded %q as the state before its call, but the first call left %q",
			sink.intents[1].PreviousHash, sink.completions[0].ResultHash)
	}
	// A target that more than one call of the batch touches cannot be described
	// up front, so neither of them is batched.
	if sink.batchWrites != 0 || sink.perCallWrites != 2 {
		t.Fatalf("batch writes = %d per-call writes = %d", sink.batchWrites, sink.perCallWrites)
	}
}

// The batch write is kept for the case it was introduced for: calls whose target
// no other call of the same batch touches.
func TestCheckpointStillBatchesDistinctTargets(t *testing.T) {
	workspace := t.TempDir()
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	sink := &orderedSink{}
	executor := &Executor{
		Registry: NewRegistry(write), Policy: policy.AllowAllChecker{}, Checkpoint: sink,
		Coordinator: NewResourceCoordinator(), MaxParallelTools: 2,
	}
	_, outcome := executor.ExecuteBatchOutcome(context.Background(), "request", []protocol.ToolCall{
		{ID: "first", Name: "write_file", Arguments: json.RawMessage(`{"path":"one.txt","content":"one","reason":"test"}`)},
		{ID: "second", Name: "write_file", Arguments: json.RawMessage(`{"path":"two.txt","content":"two","reason":"test"}`)},
	}, BatchHooks{}, 2)
	if outcome.Control != protocol.ControlContinue || outcome.Cause != nil {
		t.Fatalf("outcome = %#v", outcome)
	}
	if len(sink.intents) != 2 || sink.batchWrites != 1 || sink.perCallWrites != 0 {
		t.Fatalf("intents = %#v batch writes = %d per-call writes = %d", sink.intents, sink.batchWrites, sink.perCallWrites)
	}
}

// Two writes that share a parent directory which does not exist yet must both
// succeed: creating it is not a resource conflict, and neither call may end in an
// error state because the other created it first.
func TestCheckpointTwoWritesSharingANewParentBothSucceed(t *testing.T) {
	workspace := t.TempDir()
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	sink := &orderedSink{}
	executor := &Executor{
		Registry: NewRegistry(write), Policy: policy.AllowAllChecker{}, Checkpoint: sink,
		Coordinator: NewResourceCoordinator(), MaxParallelTools: 2,
	}
	results, outcome := executor.ExecuteBatchOutcome(context.Background(), "request", []protocol.ToolCall{
		{ID: "first", Name: "write_file", Arguments: json.RawMessage(`{"path":"shared/deep/one.txt","content":"one","create_parent_dirs":true,"reason":"test"}`)},
		{ID: "second", Name: "write_file", Arguments: json.RawMessage(`{"path":"shared/deep/two.txt","content":"two","create_parent_dirs":true,"reason":"test"}`)},
	}, BatchHooks{}, 2)
	if outcome.Control != protocol.ControlContinue || outcome.Cause != nil {
		t.Fatalf("outcome = %#v", outcome)
	}
	for index := range results {
		if results[index].IsError || results[index].State != protocol.CallSucceeded {
			t.Fatalf("results[%d] = %#v", index, results[index])
		}
	}
	for _, name := range []string{"one.txt", "two.txt"} {
		if _, err := os.Stat(filepath.Join(workspace, "shared", "deep", name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// Describing an intent observes the request context: a cancelled request must not
// create a directory, and it must not keep reading a target it will not write.
func TestIntentPreparationStopsWhenTheRequestIsCancelled(t *testing.T) {
	workspace := t.TempDir()
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "existing.txt"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if path, evidence := write.ReportIntent(ctx, json.RawMessage(`{"path":"new/deep/target.txt","create_parent_dirs":true}`)); path != "" || evidence != "" {
		t.Fatalf("a cancelled intent still described %q / %q", path, evidence)
	}
	if _, err := os.Stat(filepath.Join(workspace, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a cancelled intent created a directory")
	}
	if _, evidence := write.ReportIntent(ctx, json.RawMessage(`{"path":"existing.txt"}`)); evidence != "" {
		t.Fatalf("a cancelled intent still read its target: %q", evidence)
	}
}

// The side-effect-free resolver keeps the same guards as the writing one: a path
// that leaves the workspace through a symlink is refused, and nothing is created
// while refusing it.
func TestIntentPreparationRefusesASymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(workspace, "link")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	path, evidence := write.ReportIntent(context.Background(), json.RawMessage(`{"path":"link/target.txt","create_parent_dirs":true}`))
	if evidence != "" {
		t.Fatalf("evidence was collected through an escaping link: %q", evidence)
	}
	if path != "link/target.txt" {
		t.Fatalf("the unresolved path should be reported as given, got %q", path)
	}
	if _, err := os.Stat(filepath.Join(outside, "target.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a link that escapes the workspace reached the outside directory")
	}
}

// Read-only calls need no lifecycle record, and a file tool reports verifiable
// recovery hints.
func TestCheckpointOnlyCoversSideEffectingCalls(t *testing.T) {
	workspace := t.TempDir()
	read, err := NewReadFile(workspace, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "target.txt"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	sink := &faultSink{}
	executor := &Executor{Registry: NewRegistry(read, write), Policy: policy.AllowAllChecker{}, Checkpoint: sink}
	executor.ExecuteBatchOutcome(context.Background(), "request", []protocol.ToolCall{
		{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"target.txt"}`)},
		{ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"path":"target.txt","content":"replaced","reason":"test"}`)},
	}, BatchHooks{}, 0)
	if len(sink.intents) != 1 || sink.intents[0].CallID != "write" {
		t.Fatalf("intents = %#v", sink.intents)
	}
	intent := sink.intents[0]
	if intent.TargetPath == "" || !strings.HasSuffix(intent.TargetPath, "target.txt") {
		t.Fatalf("intent target = %q", intent.TargetPath)
	}
	// The pre-write hash is the evidence a human uses to check whether an
	// interrupted write happened.
	if intent.PreviousHash == "" {
		t.Fatal("the intent carries no previous hash")
	}
	if len(sink.completions) != 1 || sink.completions[0].ResultHash == intent.PreviousHash || sink.completions[0].ResultHash == "" {
		t.Fatalf("completions = %#v", sink.completions)
	}
	if sink.completions[0].State != protocol.CallSucceeded {
		t.Fatalf("completion state = %q", sink.completions[0].State)
	}
}

// The executor keeps working when no checkpoint sink is configured.
func TestCheckpointIsOptional(t *testing.T) {
	workspace := t.TempDir()
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	executor := &Executor{Registry: NewRegistry(write), Policy: policy.AllowAllChecker{}}
	results, outcome := executor.ExecuteBatchOutcome(context.Background(), "request", []protocol.ToolCall{{
		ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"path":"target.txt","content":"content","reason":"test"}`),
	}}, BatchHooks{}, 0)
	if outcome.Control != protocol.ControlContinue || len(results) != 1 || results[0].IsError {
		t.Fatalf("outcome=%#v results=%#v", outcome, results)
	}
}
