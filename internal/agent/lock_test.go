package agent

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	contextledger "Eylu/internal/context"
	"Eylu/internal/driver"
	"Eylu/internal/environment"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/tool"
)

// blockingDriver holds a model call open until the test releases it.
type blockingDriver struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	calls   int
}

func (d *blockingDriver) Name() string { return "blocking" }
func (d *blockingDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{}
}
func (d *blockingDriver) Generate(ctx context.Context, _ driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.once.Do(func() { close(d.started) })
	select {
	case <-d.release:
	case <-ctx.Done():
		return protocol.ModelResponse{}, ctx.Err()
	}
	d.calls++
	return textResponse("final", "done"), nil
}

// readingDriver emits one event per call, which is enough to run the host
// callback while the model call is in progress.
type readingDriver struct{ calls int }

func (*readingDriver) Name() string { return "reading" }
func (*readingDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{}
}
func (d *readingDriver) Generate(_ context.Context, _ driver.Request, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	d.calls++
	if emit != nil {
		if err := emit(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: "hello"}); err != nil {
			return protocol.ModelResponse{}, err
		}
	}
	return textResponse("agent-"+strconv.Itoa(d.calls), "done"), nil
}

// A host callback may read the conversation state: no callback runs inside the
// state lock, so this cannot deadlock.
func TestEmitCallbackCanReadConversationState(t *testing.T) {
	model := &readingDriver{}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	var reportTokens int
	var exportedTurns int
	done := make(chan error, 1)
	go func() {
		_, err := conversation.Run(context.Background(), "read-state", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 2, MaxTotalTokens: 1_000_000}, false, func(event protocol.ModelEvent) error {
			// Both of these take the state lock.
			reportTokens = conversation.ContextReport().InputTokens
			exportedTurns = len(conversation.ExportState().Turns)
			return nil
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a callback that reads the conversation deadlocked the request")
	}
	if reportTokens <= 0 || exportedTurns <= 0 {
		t.Fatalf("the callback observed no state: tokens=%d turns=%d", reportTokens, exportedTurns)
	}
}

// Context events are delivered outside the state lock too, so a context callback
// may read the conversation.
func TestContextEventCallbackCanReadConversationState(t *testing.T) {
	model := &compactionSummaryDriver{}
	conversation := compactableConversation()
	runtime := compactionUsageRuntime(model)
	var turns int
	runtime.ContextEvent = func(event contextledger.Event) {
		turns = len(conversation.ExportState().Turns)
	}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	done := make(chan error, 1)
	go func() {
		_, err := conversation.Run(context.Background(), "compact", runtime, executor, LoopOptions{MaxTurns: 1, MaxTotalTokens: 1_000_000}, false, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a context callback that reads the conversation deadlocked the request")
	}
	if turns == 0 {
		t.Fatal("the context callback never observed the transcript")
	}
}

// While the model call is blocked, a reader still sees the committed state.
func TestStateIsReadableWhileTheModelCallIsBlocked(t *testing.T) {
	model := &blockingDriver{started: make(chan struct{}), release: make(chan struct{})}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	done := make(chan error, 1)
	go func() {
		_, err := conversation.Run(context.Background(), "blocked", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 1, MaxTotalTokens: 1_000_000}, false, nil)
		done <- err
	}()
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the model call did not start")
	}
	read := make(chan int, 1)
	go func() { read <- len(conversation.ExportState().Turns) }()
	select {
	case turns := <-read:
		if turns == 0 {
			t.Fatal("the user message was not readable during the model call")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reading the conversation blocked while the model call was in flight")
	}
	close(model.release)
	if err := <-done; err != nil {
		t.Fatalf("err = %v", err)
	}
}

// A second concurrent request on the same conversation is refused, and the slot
// is free again once the first one finishes.
func TestConcurrentRunIsRefusedAndReleased(t *testing.T) {
	model := &blockingDriver{started: make(chan struct{}), release: make(chan struct{})}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	first := make(chan error, 1)
	go func() {
		_, err := conversation.Run(context.Background(), "first", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 1, MaxTotalTokens: 1_000_000}, false, nil)
		first <- err
	}()
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first request did not start")
	}
	_, err := conversation.Run(context.Background(), "second", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 1, MaxTotalTokens: 1_000_000}, false, nil)
	if !errors.Is(err, ErrConversationBusy) {
		t.Fatalf("err = %v, want ErrConversationBusy", err)
	}
	close(model.release)
	if err := <-first; err != nil {
		t.Fatalf("first err = %v", err)
	}
	// The slot is released, so the next request runs normally.
	if _, err := conversation.Run(context.Background(), "third", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 1, MaxTotalTokens: 1_000_000}, false, nil); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

// Cancelling a request releases the writer slot.
func TestCancelledRunReleasesTheSlot(t *testing.T) {
	model := &blockingDriver{started: make(chan struct{}), release: make(chan struct{})}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := conversation.Run(ctx, "cancel", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 1, MaxTotalTokens: 1_000_000}, false, nil)
		done <- err
	}()
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled request did not return")
	}
	if !conversation.gate.idle() {
		t.Fatal("the writer slot was not released after cancellation")
	}
}

// CancelRun stops the active request without touching its own context.
func TestCancelRunStopsTheActiveRequest(t *testing.T) {
	model := &blockingDriver{started: make(chan struct{}), release: make(chan struct{})}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	done := make(chan error, 1)
	go func() {
		_, err := conversation.Run(context.Background(), "tighten", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 1, MaxTotalTokens: 1_000_000}, false, nil)
		done <- err
	}()
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not start")
	}
	conversation.CancelRun()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CancelRun did not stop the request")
	}
}

// A provider change that arrives during a request is queued and applied at the
// round boundary instead of changing the running request underneath it.
func TestProviderSnapshotIsAppliedAtTheRoundBoundary(t *testing.T) {
	model := &blockingDriver{started: make(chan struct{}), release: make(chan struct{})}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	done := make(chan error, 1)
	go func() {
		_, err := conversation.Run(context.Background(), "provider", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 1, MaxTotalTokens: 1_000_000}, false, nil)
		done <- err
	}()
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not start")
	}
	updated := provider.Snapshot{Name: "updated"}
	conversation.ApplyProviderSnapshot(updated)
	// The running request has not observed the change yet.
	if pending, ok := conversation.takePendingSnapshot(); !ok || pending.Name != "updated" {
		t.Fatalf("the change was not queued: %#v", pending)
	}
	close(model.release)
	if err := <-done; err != nil {
		t.Fatalf("err = %v", err)
	}
}

// Rotating to a new session waits for the running request instead of rewriting
// state underneath it.
func TestNewSessionWaitsForTheRunningRequest(t *testing.T) {
	model := &blockingDriver{started: make(chan struct{}), release: make(chan struct{})}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	originalID := conversation.SessionID()
	done := make(chan error, 1)
	go func() {
		_, err := conversation.Run(context.Background(), "rotate", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 1, MaxTotalTokens: 1_000_000}, false, nil)
		done <- err
	}()
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not start")
	}
	rotated := make(chan string, 1)
	go func() { rotated <- conversation.NewSessionWithEnvironment(environment.Context{}) }()
	select {
	case id := <-rotated:
		t.Fatalf("the rotation did not wait for the running request (id %q)", id)
	case <-time.After(200 * time.Millisecond):
	}
	close(model.release)
	if err := <-done; err != nil {
		t.Fatalf("err = %v", err)
	}
	select {
	case id := <-rotated:
		if id != originalID {
			t.Fatalf("rotation returned %q, want the previous session %q", id, originalID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the rotation never completed")
	}
	if turns := len(conversation.ExportState().Turns); turns != 0 {
		t.Fatalf("the new session kept %d turns", turns)
	}
}
