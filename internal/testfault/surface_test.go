package testfault

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

// scriptedDriver is the thing a fault wrapper has to stay transparent around.
type scriptedDriver struct {
	calls      int
	response   protocol.ModelResponse
	err        error
	capability driver.Capabilities
	targeted   map[driver.CapabilityTarget]driver.Capabilities
}

func (d *scriptedDriver) Name() string { return "scripted" }
func (d *scriptedDriver) Capabilities() driver.Capabilities {
	return d.capability
}
func (d *scriptedDriver) CapabilitiesFor(target driver.CapabilityTarget) driver.Capabilities {
	return d.targeted[target]
}
func (d *scriptedDriver) Generate(context.Context, driver.Request, driver.EmitFunc) (protocol.ModelResponse, error) {
	d.calls++
	return d.response, d.err
}

// A fault wrapper is transparent until its schedule fires: the name, the
// capabilities and the response all come from the delegate, which is what lets a
// failing case be written against the same code path as a healthy one.
func TestDriverFaultsAreTransparentUntilTheScheduleFires(t *testing.T) {
	target := driver.CapabilityTarget{Provider: "work", Protocol: "responses", Model: "test-model"}
	delegate := &scriptedDriver{
		response:   protocol.ModelResponse{Turn: protocol.Turn{ID: "turn", Role: protocol.RoleAgent}},
		capability: driver.Capabilities{ToolCalling: true},
		targeted:   map[driver.CapabilityTarget]driver.Capabilities{target: {ToolCalling: true, ParallelTools: true}},
	}
	faults := &DriverFaults{Delegate: delegate, Fault: NewFault("driver.Generate", FirstN(1))}

	if faults.Name() != "scripted" {
		t.Fatalf("name = %q, want the delegate's", faults.Name())
	}
	if got := faults.Capabilities(); !got.ToolCalling {
		t.Fatalf("capabilities = %#v, want the delegate's", got)
	}
	// The targeted query is forwarded, not silently downgraded to the untargeted
	// capabilities: a wrapper that lost it would change what the request believes
	// the provider can do.
	if got := faults.CapabilitiesFor(target); !got.ParallelTools {
		t.Fatalf("targeted capabilities = %#v, want the delegate's", got)
	}

	// The first call is on schedule and fails without reaching the delegate.
	if _, err := faults.Generate(context.Background(), driver.Request{}, nil); err == nil || !strings.Contains(err.Error(), "driver.Generate") {
		t.Fatalf("first call error = %v, want the injected failure", err)
	}
	if delegate.calls != 0 {
		t.Fatalf("a faulted call reached the delegate: %d calls", delegate.calls)
	}
	// The schedule recovers, and the response passes through unchanged.
	response, err := faults.Generate(context.Background(), driver.Request{}, nil)
	if err != nil || response.Turn.ID != "turn" {
		t.Fatalf("recovered call = %#v, %v", response, err)
	}
	if delegate.calls != 1 {
		t.Fatalf("delegate calls = %d, want 1", delegate.calls)
	}
	if faults.Fault.Calls() != 2 {
		t.Fatalf("the fault counted %d calls, want both", faults.Fault.Calls())
	}
}

// The three failure shapes a provider can present are injectable without the
// driver or the loop holding a test-only branch: a given error, a hang that ends
// when the request's own deadline does, and a rewrite of a successful response.
func TestDriverFaultsInjectTheFailureShapesAProviderPresents(t *testing.T) {
	sentinel := &protocol.Error{Code: protocol.ErrRateLimit, Message: "slow down", Retryable: true}
	typed := &DriverFaults{Fault: NewFault("driver.Generate", Always()), Err: sentinel}
	_, err := typed.Generate(context.Background(), driver.Request{}, nil)
	var decoded *protocol.Error
	if !errors.As(err, &decoded) || decoded.Code != protocol.ErrRateLimit {
		t.Fatalf("error = %v, want the injected provider error", err)
	}

	// A hang ends with the request's deadline, never with one the fault invented.
	hung := &DriverFaults{Fault: NewFault("driver.Generate", Always()), Hang: 5 * time.Millisecond}
	started := time.Now()
	if _, err := hung.Generate(context.Background(), driver.Request{}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hang error = %v, want the context's own deadline", err)
	}
	if elapsed := time.Since(started); elapsed < 5*time.Millisecond {
		t.Fatalf("the hang returned after %s, before its own wait", elapsed)
	}

	// A rewrite sees the response the request would have received.
	delegate := &scriptedDriver{response: protocol.ModelResponse{Stop: protocol.StopCompleted, Usage: protocol.Usage{InputTokens: 3, Exact: true}}}
	rewriting := &DriverFaults{Delegate: delegate, Fault: NewFault("driver.Generate", Never()), Rewrite: func(response *protocol.ModelResponse) {
		response.Stop = protocol.StopLength
		response.Usage.Exact = false
	}}
	response, err := rewriting.Generate(context.Background(), driver.Request{}, nil)
	if err != nil || response.Stop != protocol.StopLength || response.Usage.Exact {
		t.Fatalf("rewritten response = %#v, %v", response, err)
	}

	// A delegate error is passed through untouched, and without a delegate the
	// wrapper says so rather than fabricating an empty response.
	failing := &DriverFaults{Delegate: &scriptedDriver{err: errors.New("provider refused")}, Fault: NewFault("driver.Generate", Never())}
	if _, err := failing.Generate(context.Background(), driver.Request{}, nil); err == nil || !strings.Contains(err.Error(), "provider refused") {
		t.Fatalf("delegate error = %v", err)
	}
	lonely := &DriverFaults{Fault: NewFault("driver.Generate", Never())}
	if _, err := lonely.Generate(context.Background(), driver.Request{}, nil); err == nil || !strings.Contains(err.Error(), "no delegate") {
		t.Fatalf("unwired error = %v, want a missing delegate", err)
	}
	// An unwired wrapper still answers the questions it can, so a case that only
	// wants to observe the request does not need a delegate to compile.
	if lonely.Name() != "testfault" {
		t.Fatalf("unwired name = %q", lonely.Name())
	}
	if got := lonely.Capabilities(); got.ToolCalling || got.ParallelTools {
		t.Fatalf("unwired capabilities = %#v, want none", got)
	}
	if got := lonely.CapabilitiesFor(driver.CapabilityTarget{}); got.ToolCalling {
		t.Fatalf("unwired targeted capabilities = %#v, want none", got)
	}
}

// The checkpoint surface fails either half on its own schedule and keeps every
// record, including the ones whose delivery failed, so a case can compare what
// the executor tried to record with what the store holds.
func TestCheckpointFaultsFailEachHalfAndKeepEveryRecord(t *testing.T) {
	delegate := &recordingCheckpoint{}
	faults := &CheckpointFaults{Delegate: delegate, IntentFault: NewFault("checkpoint.Intent", FirstN(1)), CompletionFault: NewFault("checkpoint.Completion", Never())}
	intent := tool.Intent{CallID: "call", Tool: "write_file", TargetPath: "one.txt"}

	if err := faults.RecordIntent(intent); err == nil || !strings.Contains(err.Error(), "checkpoint.Intent") {
		t.Fatalf("intent error = %v, want the injected failure", err)
	}
	if err := faults.RecordIntent(intent); err != nil {
		t.Fatalf("the schedule did not recover: %v", err)
	}
	if err := faults.RecordCompletion(tool.Completion{CallID: "call", Tool: "write_file"}); err != nil {
		t.Fatalf("completion error = %v", err)
	}
	if len(faults.Intents()) != 2 {
		t.Fatalf("intents = %#v, want both attempts", faults.Intents())
	}
	if len(faults.Completions()) != 1 {
		t.Fatalf("completions = %#v", faults.Completions())
	}
	// Only the delivery that was not faulted reached the delegate.
	if len(delegate.intents) != 1 || delegate.intents[0].CallID != "call" {
		t.Fatalf("delegate intents = %#v", delegate.intents)
	}
	if len(delegate.completions) != 1 {
		t.Fatalf("delegate completions = %#v", delegate.completions)
	}

	// An unwired sink records without failing, so a case only interested in what
	// the executor produced needs no delegate.
	unwired := &CheckpointFaults{}
	if err := unwired.RecordIntent(intent); err != nil {
		t.Fatalf("unwired intent error = %v", err)
	}
	if err := unwired.RecordCompletion(tool.Completion{CallID: "call"}); err != nil {
		t.Fatalf("unwired completion error = %v", err)
	}
	if len(unwired.Intents()) != 1 || len(unwired.Completions()) != 1 {
		t.Fatalf("unwired records = %#v / %#v", unwired.Intents(), unwired.Completions())
	}
}

type recordingCheckpoint struct {
	intents     []tool.Intent
	completions []tool.Completion
}

func (c *recordingCheckpoint) RecordIntent(intent tool.Intent) error {
	c.intents = append(c.intents, intent)
	return nil
}

func (c *recordingCheckpoint) RecordCompletion(completion tool.Completion) error {
	c.completions = append(c.completions, completion)
	return nil
}

// The audit surface has no error return, so a panic is the only failure it can
// raise. The record whose delivery panicked is still kept, which is what lets a
// case assert what the executor tried to report.
func TestAuditFaultsPanicOnScheduleAndKeepTheRecord(t *testing.T) {
	delegate := &recordingAudit{}
	faults := &AuditFaults{Delegate: delegate, Fault: NewFault("audit.Record", FirstN(1))}

	// The panic is not recovered here: whether a host callback may kill the
	// request is the caller's decision, so the wrapper must let it through.
	func() {
		defer func() {
			recovered := recover()
			if recovered == nil {
				t.Fatal("a scheduled audit failure did not panic")
			}
			if err, ok := recovered.(error); !ok || !strings.Contains(err.Error(), "audit.Record") {
				t.Fatalf("recovered %#v, want the injected failure", recovered)
			}
		}()
		faults.Record(tool.AuditRecord{CallID: "call", Tool: "bash"})
	}()
	if len(delegate.records) != 0 {
		t.Fatalf("a faulted record reached the delegate: %#v", delegate.records)
	}

	// After the schedule recovers the record is delivered, and both are kept.
	faults.Record(tool.AuditRecord{CallID: "call", Tool: "bash"})
	if len(faults.Records()) != 2 {
		t.Fatalf("records = %#v, want both attempts", faults.Records())
	}
	if len(delegate.records) != 1 || delegate.records[0].Tool != "bash" {
		t.Fatalf("delegate records = %#v", delegate.records)
	}
	// The returned slice is a copy: a caller cannot edit what the sink holds.
	returned := faults.Records()
	returned[0].Tool = "changed"
	if faults.Records()[0].Tool != "bash" {
		t.Fatal("the stored records were editable through the returned slice")
	}

	unwired := &AuditFaults{}
	unwired.Record(tool.AuditRecord{CallID: "call"})
	if len(unwired.Records()) != 1 {
		t.Fatalf("unwired records = %#v", unwired.Records())
	}
}

type recordingAudit struct{ records []tool.AuditRecord }

func (a *recordingAudit) Record(record tool.AuditRecord) { a.records = append(a.records, record) }

// The event surface fails delivery on schedule, can hold delivery until the test
// releases it, and counts what the host was offered either way.
func TestEventFaultsHoldAndFailDeliveryWhileCountingEveryEvent(t *testing.T) {
	var delivered []protocol.ModelEvent
	delegate := func(event protocol.ModelEvent) error {
		delivered = append(delivered, event)
		return errors.New("consumer refused")
	}
	faults := &EventFaults{Delegate: delegate, Fault: NewFault("host.Emit", Nth(2))}
	first := protocol.ModelEvent{Kind: protocol.EventTextDelta}
	second := protocol.ModelEvent{Kind: protocol.EventTextDelta}
	third := protocol.ModelEvent{Kind: protocol.EventReasoningDelta}

	if err := faults.Emit(first); err == nil || !strings.Contains(err.Error(), "consumer refused") {
		t.Fatalf("first emit = %v, want the delegate's own error", err)
	}
	if err := faults.Emit(second); err == nil || !strings.Contains(err.Error(), "host.Emit") {
		t.Fatalf("second emit = %v, want the injected failure", err)
	}
	if err := faults.Emit(third); err == nil || !strings.Contains(err.Error(), "consumer refused") {
		t.Fatalf("third emit = %v, want the delegate's own error", err)
	}
	// The second event never reached the delegate; the other two did and were
	// refused by it.
	if len(delivered) != 2 {
		t.Fatalf("delegate saw %#v", delivered)
	}
	if len(faults.Events()) != 3 {
		t.Fatalf("events = %#v, want every offer including the faulted one", faults.Events())
	}
	if got := faults.KindCount(protocol.EventTextDelta); got != 2 {
		t.Fatalf("text deltas = %d, want 2", got)
	}
	if got := faults.KindCount(protocol.EventReasoningDelta); got != 1 {
		t.Fatalf("reasoning deltas = %d, want 1", got)
	}
	if got := faults.KindCount(protocol.EventResponseDone); got != 0 {
		t.Fatalf("turn completions = %d, want 0", got)
	}

	// An unwired consumer accepts everything: a case that only wants to observe
	// the stream needs no delegate.
	unwired := &EventFaults{}
	if err := unwired.Emit(first); err != nil {
		t.Fatalf("unwired emit = %v", err)
	}

	// A barrier makes the slow-consumer window exact rather than timing-dependent:
	// the delivery is held until the test releases it.
	hold := make(chan struct{})
	held := &EventFaults{Delegate: func(protocol.ModelEvent) error { return nil }, Hold: hold}
	done := make(chan error, 1)
	go func() { done <- held.Emit(first) }()
	select {
	case err := <-done:
		t.Fatalf("a held delivery finished early: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(hold)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("released emit = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the released delivery never finished")
	}
	if len(held.Events()) != 1 {
		t.Fatalf("held events = %#v", held.Events())
	}
}

// A fault surface is what lets a case prove the code under test notices a
// failure, so each wrapper has to be assignable to the interface it replaces; a
// wrapper that was not could only be used through a type assertion.
func TestFaultSurfacesSatisfyTheInterfacesTheyReplace(t *testing.T) {
	var (
		_ driver.ModelDriver            = (*DriverFaults)(nil)
		_ driver.TargetCapabilityDriver = (*DriverFaults)(nil)
		_ driver.EmitFunc               = (&EventFaults{}).Emit
		_ tool.AuditSink                = (*AuditFaults)(nil)
		_ tool.CheckpointSink           = (*CheckpointFaults)(nil)
		_ Store                         = (*StoreFaults)(nil)
	)
}

// A schedule is a value a case composes, and an unset one is tolerated by the
// combinators rather than panicking: a case that only wants to observe one half
// of a path must not have to build a schedule for the other.
//
// The distinction is worth pinning because a nil function value is not callable
// on its own, so the tolerance has to live in the combinators and in the fault
// rather than in the caller.
func TestSchedulesAreValuesAndAnUnsetOneFailsNothing(t *testing.T) {
	var unset Schedule
	if got := Any(unset, Nth(1)); !got(1) || got(2) {
		t.Fatal("Any did not compose around an unset schedule")
	}
	if got := All(unset); got(1) {
		t.Fatal("All treated an unset schedule as always")
	}
	if got := Any(unset, nil); got(1) {
		t.Fatal("Any fired for a list of unset schedules")
	}
	if !All(Always())(1) || All()(1) {
		t.Fatal("All disagrees with itself")
	}
	// A fault with no schedule never fails and still counts what it was asked.
	fault := NewFault("dependency", nil)
	for call := 1; call <= 3; call++ {
		if err := fault.Fail(); err != nil {
			t.Fatalf("a fault with no schedule failed on call %d: %v", call, err)
		}
	}
	if fault.Calls() != 3 || fault.Failures() != 0 {
		t.Fatalf("calls = %d failures = %d", fault.Calls(), fault.Failures())
	}
}
