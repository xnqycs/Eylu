package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

// roundsDriver answers every round with one tool call and refuses to answer once
// its context is done, which is what a real provider does.
type roundsDriver struct{ issued int }

func (*roundsDriver) Name() string { return "rounds" }
func (*roundsDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true}
}

func (d *roundsDriver) Generate(ctx context.Context, _ driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return protocol.ModelResponse{}, err
	}
	d.issued++
	call := protocol.ToolCall{ID: "call-" + strconv.Itoa(d.issued), Name: "echo", Arguments: json.RawMessage(`{}`)}
	return toolUseResponse("agent-"+strconv.Itoa(d.issued), call), nil
}

// batchDriver answers the first round with the calls it was given and then
// completes.
type batchDriver struct {
	calls []protocol.ToolCall
	round int
}

func (*batchDriver) Name() string { return "batch" }
func (*batchDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true, ParallelTools: true}
}

func (d *batchDriver) Generate(ctx context.Context, _ driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return protocol.ModelResponse{}, err
	}
	d.round++
	if d.round > 1 {
		return textResponse("agent-done", "done"), nil
	}
	return toolUseResponse("agent-batch", d.calls...), nil
}

// gatedTool reports when it is provably running and then waits, so a case can put
// a safety change in the middle of a call instead of sleeping to guess at the
// window.
type gatedTool struct {
	name    string
	risk    policy.Risk
	started chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func newGatedTool(name string, risk policy.Risk) *gatedTool {
	return &gatedTool{name: name, risk: risk, started: make(chan struct{})}
}

func (g *gatedTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: g.name, Description: g.name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (g *gatedTool) Risk() policy.Risk { return g.risk }

func (g *gatedTool) Execute(ctx context.Context, _ json.RawMessage) protocol.ToolResult {
	g.calls.Add(1)
	g.once.Do(func() { close(g.started) })
	select {
	case <-g.release:
		return protocol.ToolResult{Content: "released"}
	case <-ctx.Done():
		return protocol.ToolResult{Content: "cancelled: " + ctx.Err().Error(), IsError: true}
	}
}

func (g *gatedTool) Executions() int { return int(g.calls.Load()) }

// stateOfCall returns the terminal state recorded for one call.
func stateOfCall(t *testing.T, conversation *Conversation, callID string) protocol.CallState {
	t.Helper()
	for _, turn := range conversation.Transcript() {
		for _, part := range turn.Parts {
			if part.ToolResult != nil && part.ToolResult.CallID == callID {
				return part.ToolResult.State
			}
		}
	}
	t.Fatalf("call %q has no recorded result", callID)
	return ""
}

// A setting that narrows while a request is running reaches that request at its
// next round boundary: the work already done is kept, the batch that had not
// started does not run, and the report says why the request stopped instead of
// reporting a bare cancellation.
func TestTighteningStopsTheRequestAtTheNextRoundBoundary(t *testing.T) {
	item := &countingEchoTool{}
	model := &roundsDriver{}
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	report := RunReport{}
	rounds := 0
	_, err := conversation.Run(context.Background(), "tighten", testRuntime(model, 1), executor, LoopOptions{
		MaxTurns: 5, MaxTotalTokens: 1_000_000, Report: &report,
		BeforeModel: func() string {
			rounds++
			if rounds == 2 {
				// The host narrows a safety setting at the round boundary.
				if !conversation.RequestStop("permission mode full -> plan") {
					t.Error("no request was running when the setting narrowed")
				}
			}
			return ""
		},
	}, false, nil)

	var stopErr *PolicyStopError
	if !errors.As(err, &stopErr) {
		t.Fatalf("err = %v, want a policy stop", err)
	}
	if stopErr.Reason != "permission mode full -> plan" {
		t.Fatalf("reason = %q", stopErr.Reason)
	}
	if report.StopReason != "policy_tightened" {
		t.Fatalf("stop reason = %q, want policy_tightened", report.StopReason)
	}
	if got := item.calls.Load(); got != 1 {
		t.Fatalf("tool calls = %d, want only the batch that had already run", got)
	}
	if model.issued != 1 {
		t.Fatalf("model calls = %d, want only the round that had already run", model.issued)
	}
	// The committed call keeps its result, so the history stays usable.
	if state := stateOfCall(t, conversation, "call-1"); state != protocol.CallSucceeded {
		t.Fatalf("state = %q, want the completed call kept", state)
	}
	if _, err := RestoreConversation(conversation.ExportState()); err != nil {
		t.Fatalf("restore = %v", err)
	}
	if report.ToolCalls != 1 {
		t.Fatalf("report tool calls = %d", report.ToolCalls)
	}
}

// A narrowing that arrives while a call is running converges that call by its own
// contract and starts nothing else: the calls behind it are closed without being
// executed.
func TestTighteningDuringABatchStopsTheCallsBehindIt(t *testing.T) {
	running := newGatedTool("blocked", policy.RiskRead)
	queued := newGatedTool("queued", policy.RiskRead)
	model := &batchDriver{calls: []protocol.ToolCall{
		{ID: "one", Name: "blocked", Arguments: json.RawMessage(`{}`)},
		{ID: "two", Name: "queued", Arguments: json.RawMessage(`{}`)},
	}}
	executor := &tool.Executor{
		Registry: tool.NewRegistry(running, queued), Policy: policy.AllowAllChecker{},
		// Serial execution makes the second call observably "not started yet"
		// while the first one is in flight.
		MaxParallelTools: 1,
	}
	conversation := NewConversation()
	report := RunReport{}
	done := make(chan error, 1)
	go func() {
		_, err := conversation.Run(context.Background(), "tighten", testRuntime(model, 1), executor,
			LoopOptions{MaxTurns: 3, MaxTotalTokens: 1_000_000, Report: &report}, false, nil)
		done <- err
	}()

	select {
	case <-running.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first call never started")
	}
	if !conversation.RequestStop("permission mode auto -> plan") {
		t.Fatal("no request was running when the setting narrowed")
	}
	select {
	case err := <-done:
		var stopErr *PolicyStopError
		if !errors.As(err, &stopErr) {
			t.Fatalf("err = %v, want a policy stop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not stop after the setting narrowed")
	}

	if got := running.Executions(); got != 1 {
		t.Fatalf("the running call executed %d times, want once", got)
	}
	if got := queued.Executions(); got != 0 {
		t.Fatalf("a call started after the setting narrowed: %d executions", got)
	}
	if state := stateOfCall(t, conversation, "one"); state != protocol.CallCancelled {
		t.Fatalf("running call state = %q, want cancelled", state)
	}
	if state := stateOfCall(t, conversation, "two"); state != protocol.CallNotExecuted {
		t.Fatalf("queued call state = %q, want not_executed", state)
	}
	if report.StopReason != "policy_tightened" {
		t.Fatalf("stop reason = %q", report.StopReason)
	}
	if _, err := RestoreConversation(conversation.ExportState()); err != nil {
		t.Fatalf("restore = %v", err)
	}
}

// A narrowing that arrives while an approval is pending must not start the call
// the approval was for, even when the approval comes back approved.
func TestTighteningWhileWaitingForApprovalStartsNothing(t *testing.T) {
	item := newGatedTool("guarded", policy.RiskWrite)
	model := &batchDriver{calls: []protocol.ToolCall{
		{ID: "one", Name: "guarded", Arguments: json.RawMessage(`{}`)},
	}}
	waiting := make(chan struct{})
	release := make(chan struct{})
	executor := &tool.Executor{
		Registry: tool.NewRegistry(item),
		Policy:   policy.NewChecker(policy.DefaultConfig(policy.ModeManual)),
		Confirm: func(context.Context, policy.Request, policy.Outcome) (tool.Confirmation, error) {
			close(waiting)
			// The approval is released by the test after the setting narrowed, so
			// the ordering never depends on which channel wins a race.
			<-release
			return tool.Confirmation{Approved: true}, nil
		},
	}
	conversation := NewConversation()
	report := RunReport{}
	done := make(chan error, 1)
	go func() {
		_, err := conversation.Run(context.Background(), "tighten", testRuntime(model, 1), executor,
			LoopOptions{MaxTurns: 3, MaxTotalTokens: 1_000_000, Report: &report}, false, nil)
		done <- err
	}()

	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("the approval was never requested")
	}
	if !conversation.RequestStop("permission mode manual -> plan") {
		t.Fatal("no request was running when the setting narrowed")
	}
	close(release)

	select {
	case err := <-done:
		var stopErr *PolicyStopError
		if !errors.As(err, &stopErr) {
			t.Fatalf("err = %v, want a policy stop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not stop after the setting narrowed")
	}
	if got := item.Executions(); got != 0 {
		t.Fatalf("an approved call ran after the setting narrowed: %d executions", got)
	}
	if state := stateOfCall(t, conversation, "one"); state != protocol.CallNotExecuted {
		t.Fatalf("state = %q, want not_executed", state)
	}
	if report.NotExecuted != 1 || report.StopReason != "policy_tightened" {
		t.Fatalf("report = %#v", report)
	}
}

// A narrowing between requests belongs to the next one: it neither carries a
// stop reason into that request nor pretends a request was running.
func TestStopRequestBetweenRequestsDoesNotLeakIntoTheNextOne(t *testing.T) {
	conversation := NewConversation()
	if conversation.RequestStop("permission mode full -> plan") {
		t.Fatal("a stop request reported a running request that does not exist")
	}
	model := &funcDriver{name: "plain", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		return textResponse("agent-1", "done"), nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	report := RunReport{}
	if _, err := conversation.Run(context.Background(), "plain", testRuntime(model, 1), executor,
		LoopOptions{MaxTurns: 1, MaxTotalTokens: 1_000_000, Report: &report}, false, nil); err != nil {
		t.Fatalf("err = %v", err)
	}
	if report.StopReason != string(protocol.StopCompleted) || report.Interop != nil {
		t.Fatalf("report = %#v", report)
	}
}

// The notice a host shows for this outcome never claims the request failed.
func TestPolicyStopErrorExplainsItself(t *testing.T) {
	err := &PolicyStopError{Reason: "permission mode full -> plan", Cause: context.Canceled}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("the cancellation that carried the stop was dropped")
	}
	if !strings.Contains(err.Error(), "full -> plan") {
		t.Fatalf("message = %q", err.Error())
	}
}
