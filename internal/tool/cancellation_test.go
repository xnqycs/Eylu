package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

// cancellingOnStartTool records how many calls reached Execute.
type countingSharedTool struct {
	calls atomic.Int32
}

func (t *countingSharedTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: "counting", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (t *countingSharedTool) Risk() policy.Risk  { return policy.RiskRead }
func (t *countingSharedTool) ParallelSafe() bool { return true }
func (t *countingSharedTool) Execute(context.Context, json.RawMessage) protocol.ToolResult {
	t.calls.Add(1)
	return protocol.ToolResult{Content: "ran"}
}

// A cancellation observed inside OnStart must prevent the call from starting.
func TestExecuteBatchStopsStartingCallsWhenOnStartCancels(t *testing.T) {
	item := &countingSharedTool{}
	audit := &memoryAudit{}
	executor := &Executor{Registry: NewRegistry(item), Policy: policy.AllowAllChecker{}, MaxParallelTools: 4, Audit: audit}
	ctx, cancel := context.WithCancel(context.Background())
	starts := 0
	results, err := executor.ExecuteBatch(ctx, "request", []protocol.ToolCall{
		{ID: "one", Name: "counting", Arguments: json.RawMessage(`{}`)},
		{ID: "two", Name: "counting", Arguments: json.RawMessage(`{}`)},
		{ID: "three", Name: "counting", Arguments: json.RawMessage(`{}`)},
	}, BatchHooks{OnStart: func(protocol.ToolCall) error {
		starts++
		cancel()
		return nil
	}})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	// Every call is announced exactly once and none of them reaches Execute.
	if starts != 3 || item.calls.Load() != 0 {
		t.Fatalf("starts = %d calls = %d", starts, item.calls.Load())
	}
	if len(results) != 3 {
		t.Fatalf("results = %#v", results)
	}
	for _, result := range results {
		if !result.IsError || !strings.Contains(result.Content, "cancel") {
			t.Fatalf("result = %#v", result)
		}
	}
	if len(audit.records) != 3 {
		t.Fatalf("audit records = %d, want one per call", len(audit.records))
	}
}

// A call waiting on a conflicting claim must not start once the request is
// cancelled, while the already running call keeps its own result.
func TestExecuteBatchDoesNotStartQueuedCallAfterCancellation(t *testing.T) {
	item := &scheduledBatchTool{started: make(chan string, 2), release: map[string]chan struct{}{
		"running": make(chan struct{}), "queued": make(chan struct{}),
	}}
	coordinator := NewResourceCoordinator()
	executor := &Executor{
		Registry: NewRegistry(item), Policy: policy.AllowAllChecker{}, MaxParallelTools: 2, Coordinator: coordinator,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		results []protocol.ToolResult
		err     error
	}, 1)
	go func() {
		results, err := executor.ExecuteBatch(ctx, "request", []protocol.ToolCall{
			{ID: "running", Name: "scheduled", Arguments: json.RawMessage(`{"id":"running","path":"same.go","access":"write"}`)},
			{ID: "queued", Name: "scheduled", Arguments: json.RawMessage(`{"id":"queued","path":"same.go","access":"write"}`)},
		}, BatchHooks{})
		done <- struct {
			results []protocol.ToolResult
			err     error
		}{results, err}
	}()
	expectBatchStart(t, item.started, "running")
	cancel()
	select {
	case outcome := <-done:
		if !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("err = %v", outcome.err)
		}
		if len(outcome.results) != 2 {
			t.Fatalf("results = %#v", outcome.results)
		}
		if outcome.results[0].CallID != "running" || !outcome.results[0].IsError {
			t.Fatalf("running result = %#v", outcome.results[0])
		}
		if outcome.results[1].CallID != "queued" || !outcome.results[1].IsError || !strings.Contains(outcome.results[1].Content, "cancel") {
			t.Fatalf("queued result = %#v", outcome.results[1])
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled batch did not converge")
	}
	assertNoBatchStart(t, item.started)
	// Every claim must be released, so a follow-up conflicting call is granted.
	release, err := coordinator.Acquire(context.Background(), ConcurrencySpec{
		Mode: ConcurrencyClaimed, Claims: []ResourceClaim{{Kind: ResourceFile, Path: "same.go", Access: ResourceWrite}},
	})
	if err != nil {
		t.Fatalf("conflicting claim after cancellation = %v", err)
	}
	release()
}

// cancelledAfterCommitTool reports success after triggering the cancellation,
// which models a tool that committed a side effect and only then observed the
// cancellation.
type cancelledAfterCommitTool struct {
	cancel    context.CancelFunc
	calls     atomic.Int32
	committed atomic.Int32
}

func (t *cancelledAfterCommitTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: "commit", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (t *cancelledAfterCommitTool) Risk() policy.Risk { return policy.RiskWrite }
func (t *cancelledAfterCommitTool) Execute(context.Context, json.RawMessage) protocol.ToolResult {
	t.calls.Add(1)
	// The side effect is committed before the cancellation becomes visible.
	t.committed.Add(1)
	t.cancel()
	return protocol.ToolResult{Content: "wrote 12 bytes", Metadata: map[string]any{"bytes": 12}}
}

// A committed side effect must never be rewritten as cancelled, while the
// request itself still reports the cancellation.
func TestExecuteBatchKeepsCommittedResultAfterCancellation(t *testing.T) {
	item := &cancelledAfterCommitTool{}
	ctx, cancel := context.WithCancel(context.Background())
	item.cancel = cancel
	executor := &Executor{Registry: NewRegistry(item), Policy: policy.AllowAllChecker{}}
	results, err := executor.ExecuteBatch(ctx, "request", []protocol.ToolCall{
		{ID: "commit", Name: "commit", Arguments: json.RawMessage(`{}`)},
	}, BatchHooks{})
	if item.calls.Load() != 1 || item.committed.Load() != 1 {
		t.Fatalf("calls = %d committed = %d", item.calls.Load(), item.committed.Load())
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(results) != 1 || results[0].IsError || results[0].Content != "wrote 12 bytes" {
		t.Fatalf("committed result was overwritten: %#v", results)
	}
}

// An approval-channel failure is infrastructure, not a user decision: the whole
// preflight batch stops and no call runs.
func TestExecuteBatchApprovalChannelFailureAbortsBatch(t *testing.T) {
	item := &confirmedBatchTool{}
	confirmations := 0
	executor := &Executor{
		Registry: NewRegistry(item), Policy: policy.NewChecker(policy.DefaultConfig(policy.ModeManual)), MaxParallelTools: 3,
		Confirm: func(context.Context, policy.Request, policy.Outcome) (Confirmation, error) {
			confirmations++
			if confirmations == 2 {
				return Confirmation{}, errors.New("approval channel closed")
			}
			return Confirmation{Approved: true}, nil
		},
	}
	results, err := executor.ExecuteBatch(context.Background(), "request", []protocol.ToolCall{
		{ID: "approved", Name: "confirmed", Arguments: json.RawMessage(`{}`)},
		{ID: "channel", Name: "confirmed", Arguments: json.RawMessage(`{}`)},
		{ID: "remaining", Name: "confirmed", Arguments: json.RawMessage(`{}`)},
	}, BatchHooks{})
	var approvalErr *ApprovalError
	if !errors.As(err, &approvalErr) || !strings.Contains(approvalErr.Error(), "approval channel closed") {
		t.Fatalf("err = %v", err)
	}
	if item.calls.Load() != 0 || confirmations != 2 {
		t.Fatalf("calls = %d confirmations = %d", item.calls.Load(), confirmations)
	}
	if len(results) != 3 {
		t.Fatalf("results = %#v", results)
	}
	if !strings.Contains(results[1].Content, "confirmation failed") {
		t.Fatalf("approval failure result = %#v", results[1])
	}
	for _, index := range []int{0, 2} {
		if !results[index].IsError || !strings.Contains(results[index].Content, "cancel") {
			t.Fatalf("result[%d] = %#v", index, results[index])
		}
		if results[index].Metadata["interrupt_request"] == true {
			t.Fatalf("result[%d] reported a user interruption: %#v", index, results[index])
		}
	}
}

// The first substantive hook failure stays the leading error, and a request
// cancellation is preserved beside it so errors.Is holds for both.
func TestExecuteBatchKeepsHookFailureAndCancellation(t *testing.T) {
	item := &countingSharedTool{}
	sentinel := errors.New("result sink closed")
	executor := &Executor{Registry: NewRegistry(item), Policy: policy.AllowAllChecker{}, MaxParallelTools: 2}
	ctx, cancel := context.WithCancel(context.Background())
	results, err := executor.ExecuteBatch(ctx, "request", []protocol.ToolCall{
		{ID: "one", Name: "counting", Arguments: json.RawMessage(`{}`)},
		{ID: "two", Name: "counting", Arguments: json.RawMessage(`{}`)},
	}, BatchHooks{OnResult: func(protocol.ToolResult) error {
		cancel()
		return sentinel
	}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the hook failure", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation cause too", err)
	}
	if !strings.HasPrefix(err.Error(), sentinel.Error()) {
		t.Fatalf("err message does not lead with the hook failure: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %#v", results)
	}
}

func TestExecuteBatchOnStartFailureKeepsCancellation(t *testing.T) {
	item := &countingSharedTool{}
	executor := &Executor{Registry: NewRegistry(item), Policy: policy.AllowAllChecker{}}
	results, err := executor.ExecuteBatch(context.Background(), "request", []protocol.ToolCall{
		{ID: "one", Name: "counting", Arguments: json.RawMessage(`{}`)},
	}, BatchHooks{OnStart: func(protocol.ToolCall) error { return context.Canceled }})
	if !errors.Is(err, context.Canceled) || item.calls.Load() != 0 {
		t.Fatalf("err = %v calls = %d", err, item.calls.Load())
	}
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("results = %#v", results)
	}
}

// An already cancelled request must not receive a resource claim.
func TestResourceCoordinatorRejectsCancelledAcquire(t *testing.T) {
	coordinator := NewResourceCoordinator()
	spec := ConcurrencySpec{Mode: ConcurrencyClaimed, Claims: []ResourceClaim{{Kind: ResourceFile, Path: "same.go", Access: ResourceWrite}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if release, err := coordinator.Acquire(ctx, spec); err == nil {
		release()
		t.Fatal("cancelled context received a claim")
	} else if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	release, err := coordinator.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatalf("follow-up acquire = %v", err)
	}
	release()
}

// A cancelled waiter must be removed from the queue so later conflicting callers
// are not blocked behind it.
func TestResourceCoordinatorRemovesWaiterOnCancel(t *testing.T) {
	coordinator := NewResourceCoordinator()
	spec := ConcurrencySpec{Mode: ConcurrencyClaimed, Claims: []ResourceClaim{{Kind: ResourceFile, Path: "same.go", Access: ResourceWrite}}}
	holding, err := coordinator.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waited := make(chan error, 1)
	go func() {
		release, err := coordinator.Acquire(ctx, spec)
		if err == nil {
			release()
		}
		waited <- err
	}()
	waitForBatchEvent(t, func() bool {
		coordinator.mu.Lock()
		defer coordinator.mu.Unlock()
		return len(coordinator.waiters) == 1
	})
	cancel()
	select {
	case err := <-waited:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter err = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter did not return")
	}
	holding()
	// No stale waiter may remain: this conflicting call must be granted at once.
	release, err := coordinator.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatalf("acquire after waiter removal = %v", err)
	}
	release()
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if len(coordinator.waiters) != 0 || len(coordinator.active) != 0 {
		t.Fatalf("waiters = %d active = %d", len(coordinator.waiters), len(coordinator.active))
	}
}

func TestWriteFileCancellationLeavesTargetUntouched(t *testing.T) {
	workspace := t.TempDir()
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "target.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := write.Execute(ctx, json.RawMessage(`{"path":"target.txt","content":"replaced","reason":"test"}`))
	if !result.IsError || !strings.Contains(result.Content, "cancel") {
		t.Fatalf("result = %#v", result)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "original" {
		t.Fatalf("target = %q err = %v", data, err)
	}
	assertNoWriteArtifacts(t, workspace)
}

func TestEditFileCancellationLeavesTargetUntouched(t *testing.T) {
	workspace := t.TempDir()
	edit, err := NewEditFile(workspace, 0)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "target.txt")
	if err := os.WriteFile(target, []byte("original content"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := edit.Execute(ctx, json.RawMessage(`{"path":"target.txt","old_string":"original","new_string":"changed","reason":"test"}`))
	if !result.IsError || !strings.Contains(result.Content, "cancel") {
		t.Fatalf("result = %#v", result)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "original content" {
		t.Fatalf("target = %q err = %v", data, err)
	}
	assertNoWriteArtifacts(t, workspace)
}

func TestWriteFileAtomicallyCleansUpTemporaryArtifact(t *testing.T) {
	workspace := t.TempDir()
	target := filepath.Join(workspace, "occupied")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomically(context.Background(), target, []byte("content"), 0o644); err == nil {
		t.Skip("this platform replaces a directory with a file")
	}
	assertNoWriteArtifacts(t, workspace)

	succeeded := filepath.Join(workspace, "written.txt")
	if err := writeFileAtomically(context.Background(), succeeded, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertNoWriteArtifacts(t, workspace)
}

// A cancellation raised inside OnStart must stop the write before any side
// effect, leaving the target untouched and no temporary artifact behind.
func TestExecuteBatchOnStartCancellationLeavesFileUntouched(t *testing.T) {
	workspace := t.TempDir()
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "target.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	executor := &Executor{Registry: NewRegistry(write), Policy: policy.AllowAllChecker{}}
	results, outcome := executor.ExecuteBatchOutcome(ctx, "request", []protocol.ToolCall{{
		ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"path":"target.txt","content":"replaced","reason":"test"}`),
	}}, BatchHooks{OnStart: func(protocol.ToolCall) error {
		cancel()
		return nil
	}}, 0)
	if outcome.Control != protocol.ControlCancelRequest || !errors.Is(outcome.Err(), context.Canceled) {
		t.Fatalf("outcome = %#v", outcome)
	}
	if len(results) != 1 || results[0].State != protocol.CallNotExecuted || !results[0].IsError {
		t.Fatalf("results = %#v", results)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "original" {
		t.Fatalf("target = %q err = %v", data, err)
	}
	assertNoWriteArtifacts(t, workspace)
}

func assertNoWriteArtifacts(t *testing.T, workspace string) {
	t.Helper()
	entries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".eylu-write-") {
			t.Fatalf("temporary write artifact left behind: %s", entry.Name())
		}
	}
}
