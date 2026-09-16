package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

// faultSink records lifecycle calls and can fail either side.
type faultSink struct {
	intents     []Intent
	completions []Completion
	intentErr   error
	resultErr   error
}

func (s *faultSink) RecordIntent(intent Intent) error {
	if s.intentErr != nil {
		return s.intentErr
	}
	s.intents = append(s.intents, intent)
	return nil
}

func (s *faultSink) RecordCompletion(completion Completion) error {
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
