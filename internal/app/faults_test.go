package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/session"
	"Eylu/internal/testfault"
	"Eylu/internal/tool"
)

// sessionStore is the explicit form of the two durable seams on sessionRuntime.
// testfault.Store implements it, so one object can fail the event log and the
// snapshot together instead of two closures a test has to keep in step.
type sessionStore interface {
	Append(string, []session.Event) ([]session.Event, error)
	Save(session.Snapshot) error
}

// installStore routes the durable half of the runtime through an explicit store.
// Production leaves both seams unset and the session store is used directly.
func installStore(runtime *sessionRuntime, store sessionStore) {
	runtime.appendEvents = store.Append
	runtime.saveSnapshot = store.Save
}

// runAndRecover runs one request and reports a panic it raised instead of
// letting it take the test binary down with it.
func runAndRecover(run func() error) (recovered any, err error) {
	defer func() { recovered = recover() }()
	err = run()
	return recovered, err
}

// heldTool blocks inside Execute until the test either releases it or cancels
// the request. It is how a case puts a fault in the middle of a call without
// sleeping to guess at the window.
type heldTool struct {
	definition protocol.ToolDefinition
	started    chan struct{}
	released   chan struct{}
	startOnce  sync.Once
	executions int32
}

func newHeldTool(name string) *heldTool {
	return &heldTool{
		definition: protocol.ToolDefinition{
			Name: name, Description: "blocks until the test releases it",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"}}}`),
		},
		started:  make(chan struct{}),
		released: make(chan struct{}),
	}
}

func (h *heldTool) Definition() protocol.ToolDefinition { return h.definition }
func (h *heldTool) Risk() policy.Risk                   { return policy.RiskWrite }

func (h *heldTool) Execute(ctx context.Context, _ json.RawMessage) protocol.ToolResult {
	atomic.AddInt32(&h.executions, 1)
	h.startOnce.Do(func() { close(h.started) })
	select {
	case <-h.released:
		return protocol.ToolResult{Content: "released"}
	case <-ctx.Done():
		return protocol.ToolResult{Content: "cancelled: " + ctx.Err().Error(), IsError: true}
	}
}

func (h *heldTool) Executions() int { return int(atomic.LoadInt32(&h.executions)) }

// The event log and the snapshot are separate host dependencies, so they can be
// down at the same time. A failed append must not be followed by a snapshot
// attempt, the recovery of the log must not duplicate a turn, and the log and
// the snapshot must converge once both are back.
func TestStoreAndSnapshotFailuresTogetherStayIdempotent(t *testing.T) {
	fixture := newSessionSyncFixture(t)
	faults := &testfault.StoreFaults{
		Delegate:    fixture.store,
		AppendFault: testfault.NewFault("store.Append", testfault.FirstN(1)),
		SaveFault:   testfault.NewFault("store.Save", testfault.FirstN(2)),
	}
	installStore(fixture.controller, faults)
	conversation := conversationWithPrompts(t, fixture.sessionID, "first")

	// The log is down, so nothing is written and the snapshot is never touched.
	if err := fixture.controller.Sync(conversation, fixture.manager, chatOptions{}, nil); err == nil {
		t.Fatal("expected the failed append to be reported")
	}
	if faults.SaveFault.Calls() != 0 {
		t.Fatalf("the snapshot was attempted although the append failed: %d calls", faults.SaveFault.Calls())
	}
	if got := countEvents(fixture.events(t), session.EventTurnAppended); got != 0 {
		t.Fatalf("a failed append still wrote %d turns", got)
	}

	// The log recovers while the snapshot stays down for two attempts.
	for attempt := 1; attempt <= 2; attempt++ {
		if err := fixture.controller.Sync(conversation, fixture.manager, chatOptions{}, nil); err == nil {
			t.Fatalf("attempt %d: expected the failed snapshot to be reported", attempt)
		}
	}
	afterFailures := fixture.events(t)
	if got := countEvents(afterFailures, session.EventTurnAppended); got != 2 {
		t.Fatalf("turn events = %d, want one per turn", got)
	}
	identifiers := make(map[string]int, len(afterFailures))
	for _, event := range afterFailures {
		identifiers[event.ID]++
	}
	for id, count := range identifiers {
		if count != 1 {
			t.Fatalf("event ID %q appears %d times after the retries", id, count)
		}
	}

	// The snapshot recovers and converges with the log without growing it.
	if err := fixture.controller.Sync(conversation, fixture.manager, chatOptions{}, nil); err != nil {
		t.Fatalf("the recovering sync failed: %v", err)
	}
	if final := fixture.events(t); len(final) != len(afterFailures) {
		t.Fatalf("the recovering sync grew the log: %d -> %d", len(afterFailures), len(final))
	}
	stored, _, err := fixture.store.Load(fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Turns) != 2 || len(stored.PromptHistory) != 1 {
		t.Fatalf("snapshot turns = %d prompts = %d", len(stored.Turns), len(stored.PromptHistory))
	}
	if fixture.controller.snapshotPending {
		t.Fatal("the snapshot is still pending after a successful save")
	}
	if faults.AppendFault.Failures() != 1 || faults.SaveFault.Failures() != 2 {
		t.Fatalf("append failures = %d, snapshot failures = %d", faults.AppendFault.Failures(), faults.SaveFault.Failures())
	}
}

// A checkpoint failure is not a tool failure: the batch stops starting side
// effects. The calls that never started are closed as not executed, the one that
// did run keeps its result, and the checkpoint failure leads the reported cause.
func TestCheckpointIntentFailureCancelsTheRestOfTheBatch(t *testing.T) {
	workspace := t.TempDir()
	write, err := tool.NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints := &testfault.CheckpointFaults{
		IntentFault: testfault.NewFault("checkpoint.RecordIntent", testfault.Nth(2)),
	}
	executor := &tool.Executor{
		Registry: tool.NewRegistry(write), Policy: policy.AllowAllChecker{},
		Checkpoint: checkpoints, Workspace: workspace, MaxParallelTools: 1,
	}
	results, outcome := executor.ExecuteBatchOutcome(context.Background(), "request", []protocol.ToolCall{
		{ID: "one", Name: "write_file", Arguments: json.RawMessage(`{"path":"one.txt","content":"one","reason":"test"}`)},
		{ID: "two", Name: "write_file", Arguments: json.RawMessage(`{"path":"two.txt","content":"two","reason":"test"}`)},
		{ID: "three", Name: "write_file", Arguments: json.RawMessage(`{"path":"three.txt","content":"three","reason":"test"}`)},
	}, tool.BatchHooks{}, 0)

	if outcome.Control != protocol.ControlAbortRequest {
		t.Fatalf("control = %q, want an aborted batch", outcome.Control)
	}
	var checkpointErr *tool.CheckpointError
	if !errors.As(outcome.Cause, &checkpointErr) || checkpointErr.Recorded {
		t.Fatalf("cause = %v, want a failed intent", outcome.Cause)
	}
	if checkpoints.IntentFault.Failures() != 1 {
		t.Fatalf("intent failures = %d, want 1", checkpoints.IntentFault.Failures())
	}
	// The first call ran and is closed; the other two never started.
	if len(results) != 3 {
		t.Fatalf("results = %#v", results)
	}
	if results[0].State != protocol.CallSucceeded || results[0].IsError {
		t.Fatalf("first result = %#v", results[0])
	}
	for index, result := range results[1:] {
		if result.State != protocol.CallNotExecuted {
			t.Fatalf("result %d state = %q, want not executed", index+2, result.State)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "one.txt")); err != nil {
		t.Fatalf("the call that ran did not leave its file: %v", err)
	}
	for _, name := range []string{"two.txt", "three.txt"} {
		if _, err := os.Stat(filepath.Join(workspace, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s exists although its intent could not be recorded", name)
		}
	}
	// Every lifecycle record that was written belongs to a call that ran.
	if got := len(checkpoints.Intents()); got != 2 {
		t.Fatalf("intents = %d, want one per call that reached execution", got)
	}
	if got := len(checkpoints.Completions()); got != 1 {
		t.Fatalf("completions = %d, want only the call that ran", got)
	}
}

// A call cancelled while it runs and a completion that cannot be recorded are
// both true at once. The result keeps the cancellation, the batch reports the
// checkpoint failure as the leading cause, and the request cancellation survives
// beside it instead of being replaced.
func TestCheckpointCompletionFailureDuringACancellationKeepsBothCauses(t *testing.T) {
	workspace := t.TempDir()
	held := newHeldTool("held")
	checkpoints := &testfault.CheckpointFaults{
		CompletionFault: testfault.NewFault("checkpoint.RecordCompletion", testfault.Always()),
	}
	executor := &tool.Executor{
		Registry: tool.NewRegistry(held), Policy: policy.AllowAllChecker{},
		Checkpoint: checkpoints, Workspace: workspace, MaxParallelTools: 1,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		results []protocol.ToolResult
		batch   protocol.BatchOutcome
	}
	done := make(chan outcome, 1)
	go func() {
		results, batch := executor.ExecuteBatchOutcome(ctx, "request", []protocol.ToolCall{
			{ID: "held", Name: "held", Arguments: json.RawMessage(`{"reason":"test"}`)},
		}, tool.BatchHooks{}, 0)
		done <- outcome{results: results, batch: batch}
	}()

	// A barrier, not a sleep: the cancellation is delivered once the call is
	// provably inside Execute.
	select {
	case <-held.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the held tool never started")
	}
	cancel()

	var finished outcome
	select {
	case finished = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the batch did not finish after the cancellation")
	}

	if held.Executions() != 1 {
		t.Fatalf("executions = %d, want exactly one", held.Executions())
	}
	if len(finished.results) != 1 || finished.results[0].State != protocol.CallCancelled {
		t.Fatalf("results = %#v, want one cancelled call", finished.results)
	}
	if finished.batch.Control != protocol.ControlAbortRequest {
		t.Fatalf("control = %q, want an aborted batch", finished.batch.Control)
	}
	var checkpointErr *tool.CheckpointError
	if !errors.As(finished.batch.Cause, &checkpointErr) || !checkpointErr.Recorded {
		t.Fatalf("cause = %v, want a failed completion", finished.batch.Cause)
	}
	if !errors.Is(finished.batch.Cause, context.Canceled) {
		t.Fatalf("cause = %v, want the request cancellation preserved beside it", finished.batch.Cause)
	}
	if len(checkpoints.Intents()) != 1 || len(checkpoints.Completions()) != 1 {
		t.Fatalf("intents = %d completions = %d, want both sides of the one call",
			len(checkpoints.Intents()), len(checkpoints.Completions()))
	}
}

// A provider timeout ends the request, and the restart that follows must see the
// side effect the tool already committed exactly once, with nothing pending and
// nothing replayed.
func TestProviderTimeoutSurvivesARestartWithoutReplaying(t *testing.T) {
	fixture := newSessionSyncFixture(t)
	write, err := tool.NewWriteFile(fixture.workspace)
	if err != nil {
		t.Fatal(err)
	}
	executor := &tool.Executor{
		Registry: tool.NewRegistry(write), Policy: policy.AllowAllChecker{},
		Checkpoint: fixture.controller, Workspace: fixture.workspace,
	}
	// The first model turn asks for the write; the second one, which would report
	// the result, never answers.
	providerDriver := &testfault.DriverFaults{
		Delegate: &writeDriver{path: "target.txt", content: "written"},
		Fault:    testfault.NewFault("driver.Generate", testfault.Nth(2)),
		Hang:     20 * time.Millisecond,
	}
	conversation := conversationWithPrompts(t, fixture.sessionID)
	runtime := agent.Runtime{
		Provider: provider.Snapshot{Name: "stub", Config: config.ProviderConfig{Adapter: "stub", BaseURL: "https://example.test/v1", Model: "stub-model"}},
		Driver:   providerDriver, Workspace: fixture.workspace, PermissionMode: "full",
	}
	_, err = conversation.Run(context.Background(), "write the file", runtime, executor,
		agent.LoopOptions{MaxTurns: 3, MaxTotalTokens: 1_000_000}, false, nil)
	if err == nil {
		t.Fatal("expected the provider timeout to be reported")
	}
	if providerDriver.Fault.Failures() != 1 {
		t.Fatalf("driver failures = %d, want the one timeout", providerDriver.Fault.Failures())
	}

	// The side effect happened once and both of its lifecycle records exist.
	data, readErr := os.ReadFile(filepath.Join(fixture.workspace, "target.txt"))
	if readErr != nil || string(data) != "written" {
		t.Fatalf("the tool did not write its file: %q err = %v", data, readErr)
	}
	events := fixture.events(t)
	if got := countEvents(events, session.EventToolExecutionIntent); got != 1 {
		t.Fatalf("intents = %d, want 1", got)
	}
	if got := countEvents(events, session.EventToolCompleted); got != 1 {
		t.Fatalf("completions = %d, want 1", got)
	}

	// The restart reads the log alone: nothing is pending and nothing is replayed.
	snapshot := reload(t, fixture)
	if len(snapshot.PendingIntents) != 0 {
		t.Fatalf("pending intents = %#v", snapshot.PendingIntents)
	}
	again, readErr := os.ReadFile(filepath.Join(fixture.workspace, "target.txt"))
	if readErr != nil || string(again) != "written" {
		t.Fatalf("the restart changed the file: %q err = %v", again, readErr)
	}
	if _, err := agent.RestoreConversation(agentStateFromSnapshot(snapshot)); err != nil {
		t.Fatalf("restore = %v", err)
	}
}

// poisonedBatchDriver asks for a call that cannot run and one that would write a
// file. A fault raised while reporting the first is therefore visible before the
// second one starts.
type poisonedBatchDriver struct{ requests int }

func (*poisonedBatchDriver) Name() string { return "stub" }
func (*poisonedBatchDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true}
}

func (d *poisonedBatchDriver) Generate(_ context.Context, _ driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.requests++
	if d.requests > 1 {
		done := protocol.Turn{Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "done"}}}
		return protocol.ModelResponse{Turn: done, Stop: protocol.StopCompleted}, nil
	}
	missing := protocol.ToolCall{ID: "missing-1", Name: "missing_tool", Arguments: json.RawMessage(`{}`)}
	write := protocol.ToolCall{ID: "write-1", Name: "write_file", Arguments: json.RawMessage(`{"path":"target.txt","content":"written","reason":"test"}`)}
	turn := protocol.Turn{Role: protocol.RoleAgent, Parts: []protocol.Part{
		{Kind: protocol.PartToolCall, ToolCall: &missing},
		{Kind: protocol.PartToolCall, ToolCall: &write},
	}}
	return protocol.ModelResponse{Turn: turn, Stop: protocol.StopToolUse}, nil
}

// Characterisation, not approval: today an audit sink panic is not isolated, so
// it escapes the request and preempts everything after it. The event sink still
// reports its own failure first, because the tool-start event is delivered
// before the audit record is written, but the request ends by panicking instead
// of reporting both faults and closing the batch. PR-18 of the hardening plan
// owns the fix and will rewrite this case into "the request finishes, the audit
// failure is counted, and the side effect still happens exactly once".
func TestAuditPanicTodayPreemptsTheEventSink(t *testing.T) {
	fixture := newSessionSyncFixture(t)
	write, err := tool.NewWriteFile(fixture.workspace)
	if err != nil {
		t.Fatal(err)
	}
	audit := &testfault.AuditFaults{Fault: testfault.NewFault("audit.Record", testfault.Always())}
	events := &testfault.EventFaults{Fault: testfault.NewFault("event.Emit", testfault.Always())}
	executor := &tool.Executor{
		Registry: tool.NewRegistry(write), Policy: policy.AllowAllChecker{},
		Audit: audit, Checkpoint: fixture.controller, Workspace: fixture.workspace,
	}
	runtime := agent.Runtime{
		Provider: provider.Snapshot{Name: "stub", Config: config.ProviderConfig{Adapter: "stub", BaseURL: "https://example.test/v1", Model: "stub-model"}},
		Driver:   &poisonedBatchDriver{}, Workspace: fixture.workspace, PermissionMode: "full",
	}
	conversation := conversationWithPrompts(t, fixture.sessionID)
	recovered, _ := runAndRecover(func() error {
		_, err := conversation.Run(context.Background(), "write the file", runtime, executor,
			agent.LoopOptions{MaxTurns: 3, MaxTotalTokens: 1_000_000}, false, events.Emit)
		return err
	})
	if recovered == nil {
		t.Fatal("the audit panic was isolated; PR-18 owns that change, so this case must be rewritten with it")
	}
	if audit.Fault.Failures() != 1 || len(audit.Records()) != 1 {
		t.Fatalf("audit failures = %d records = %d, want the one call it was told about", audit.Fault.Failures(), len(audit.Records()))
	}
	if events.Fault.Failures() != 1 {
		t.Fatalf("event failures = %d, want the tool-start delivery", events.Fault.Failures())
	}
	if events.KindCount(protocol.EventToolResult) != 0 {
		t.Fatal("a tool result reached the host although the request panicked before it")
	}
	// The panicking report belongs to the call that could not run, so the call
	// behind it never started.
	if _, err := os.Stat(filepath.Join(fixture.workspace, "target.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a side effect started after the host callback panicked")
	}
}
