package agent

import (
	"fmt"
	"strings"

	"Eylu/internal/protocol"
)

// RunReport is the durable summary of one finished request.
//
// It answers, from the record alone, why a request stopped, which executions it
// performed, and which calls never reached a terminal state. It is deliberately
// separate from the UI event stream: a critical execution record must not exist
// only in a transient event.
type RunReport struct {
	RequestID  string `json:"request_id"`
	Iterations int    `json:"iterations"`
	// StopReason is why the request ended: a model stop reason, the iteration
	// limit, the token budget, a cancellation, an interruption or a failure.
	StopReason string `json:"stop_reason"`
	// Error states the failure that ended the request, when there was one.
	Error string `json:"error,omitempty"`

	ModelCalls int `json:"model_calls"`
	// ToolCalls counts the executions of this request.
	ToolCalls int `json:"tool_calls"`
	// The remaining counters are the terminal states of this request's calls, so
	// "what ran", "what was refused" and "what is unknown" stay distinguishable.
	Succeeded      int `json:"succeeded"`
	Failed         int `json:"failed"`
	Rejected       int `json:"rejected"`
	Cancelled      int `json:"cancelled"`
	NotExecuted    int `json:"not_executed"`
	OutcomeUnknown int `json:"outcome_unknown"`

	InputTokens     int  `json:"input_tokens"`
	OutputTokens    int  `json:"output_tokens"`
	ReasoningTokens int  `json:"reasoning_tokens,omitempty"`
	ExactUsage      bool `json:"exact_usage"`
	EstimatedUsage  bool `json:"estimated_usage"`
	// CachedInputTokens is the part of InputTokens the providers served from their
	// own cache, for cost accounting. It is a subset, so the budget is unaffected.
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`

	// AuditFailures counts host audit records that could not be delivered. A
	// nonzero value means the audit trail is incomplete for this request, and the
	// request itself is unaffected.
	AuditFailures int `json:"audit_failures,omitempty"`
	// EventsDropped counts non-critical host events - streamed text and reasoning
	// deltas - that were dropped because the consumer could not keep up. The
	// transcript is complete either way; the host's view of the stream is not.
	EventsDropped int `json:"events_dropped,omitempty"`
	// Warnings lists diagnostics that did not stop the request but that a caller
	// must be able to see: a host callback that failed, a consumer that fell
	// behind, a tool call that was still open when the request ended.
	Warnings []string `json:"warnings,omitempty"`
	// PendingAtEnd counts the tool calls that were still open when the request
	// ended. The healthy value is 0: a request that ends with an open call is a
	// defect, and publishing the count is what keeps it from passing unnoticed.
	PendingAtEnd int `json:"pending_at_end,omitempty"`

	// RecoveredCalls lists tool call IDs that had no recorded result and were
	// closed with outcome_unknown when a request was built.
	RecoveredCalls []string `json:"recovered_calls,omitempty"`
	// Interop names every deliberate relaxation of the provider interoperability
	// policy this request used, so a relaxed request is never indistinguishable
	// from a conforming one. An empty list is the normal case.
	Interop []string `json:"interop,omitempty"`
}

// recordInterop adds the relaxations a provider needed to the report, keeping
// the order they were observed in and never repeating one.
func recordInterop(report *RunReport, notes []string) {
	if report == nil {
		return
	}
	for _, note := range notes {
		if note == "" {
			continue
		}
		seen := false
		for _, existing := range report.Interop {
			if existing == note {
				seen = true
				break
			}
		}
		if !seen {
			report.Interop = append(report.Interop, note)
		}
	}
}

func (r *RunReport) count(state protocol.CallState) {
	switch state {
	case protocol.CallSucceeded:
		r.Succeeded++
	case protocol.CallFailed:
		r.Failed++
	case protocol.CallRejected:
		r.Rejected++
	case protocol.CallCancelled:
		r.Cancelled++
	case protocol.CallNotExecuted:
		r.NotExecuted++
	case protocol.CallOutcomeUnknown:
		r.OutcomeUnknown++
	}
}

// Reasons a request can end with, used in the report when the model did not
// supply the reason itself.
const (
	stopIterationLimit = "iteration_limit"
	stopTokenBudget    = "token_budget"
	stopCancelled      = "cancelled"
	stopAborted        = "aborted"
	stopInterrupted    = "interrupt_request"
	// stopPersistenceFailed is the reason a request whose committed turn could not
	// be made durable gets. The work already done is kept; nothing new starts.
	stopPersistenceFailed = "persistence_failed"
	// eventSinkFailedStop is the reason a request whose host event consumer failed
	// gets. It is the one host-callback failure that still ends a request, because a
	// critical event that cannot be delivered leaves the host's view of the request
	// wrong rather than merely incomplete.
	eventSinkFailedStop = "event_sink_failed"
	// stopPolicyTightened is the reason a host that narrowed a safety setting
	// gets: the request stopped because it would otherwise have run under the
	// settings that were just replaced.
	stopPolicyTightened = "policy_tightened"
)

// PolicyStopError reports that a request ended because its host narrowed a safety
// setting while it was running.
//
// It is not a failure of the request: the work already done is kept and the
// results already obtained are preserved, so a caller presents it as "stopped on
// the new settings" rather than as an error, and the run report says
// policy_tightened with the reason.
type PolicyStopError struct {
	// Reason names the setting that narrowed, in the words of the host.
	Reason string
	// Cause is the cancellation that carried the stop, when there was one.
	Cause error
}

func (e *PolicyStopError) Error() string {
	if e.Reason == "" {
		return "the request stopped because the safety settings were tightened"
	}
	return "the request stopped because the safety settings were tightened: " + e.Reason
}

func (e *PolicyStopError) Unwrap() error { return e.Cause }

// runFinalizer owns the exit paths of one request.
//
// It closes the calls that never reached a terminal state, publishes the
// accumulated usage and fills the run report. Every return of Run goes through
// it, so no committed call can be left dangling and no request can end without a
// stated reason.
type runFinalizer struct {
	conversation *Conversation
	runtime      Runtime
	calls        []protocol.ToolCall
	budget       *BudgetTracker
	usage        *RunUsage
	report       *RunReport
	iterations   int
	// events is the bounded host event queue of this request, when one is active.
	events *eventQueue
	// auditFailures reports how many host audit records this request could not
	// deliver, and auditDetail describes the most recent one.
	auditFailures func() int
	auditDetail   func() string
}

// closePending gives every call of the current response that has no result yet a
// terminal state. A call that already has a result is never rewritten.
func (f *runFinalizer) closePending(message string) {
	f.conversation.mu.Lock()
	defer f.conversation.mu.Unlock()
	f.conversation.closeToolCalls(f.runtime, f.calls, protocol.CallNotExecuted, message)
}

// finish publishes the request outcome and returns what the caller receives.
// A failure returns the last usable response beside the error, so a caller that
// stops still sees the work that happened.
//
// The host event queue is stopped here, on every exit path, so the events of this
// request are always drained and a consumer failure is never lost: when the
// request itself succeeded, the sink failure becomes its error.
func (f *runFinalizer) finish(response protocol.ModelResponse, last protocol.ModelResponse, err error, stop string) (protocol.ModelResponse, error) {
	if sinkErr := f.events.stop(); sinkErr != nil && err == nil {
		err = sinkErr
		stop = eventSinkFailedStop
	}
	// A host that narrowed a safety setting owns the reason this request stopped
	// for: the cancellation is only how it got there. A request that finished on
	// its own is never rewritten, so a stop request that arrives at the very end
	// cannot turn a completion into a failure.
	if reason, ok := f.conversation.takePolicyStop(); ok && (stop == stopCancelled || stop == stopAborted) {
		stop = stopPolicyTightened
		err = &PolicyStopError{Reason: reason, Cause: err}
	}
	f.publish(stop, err)
	if err != nil {
		return last, err
	}
	return response, nil
}

// publish records the accumulated usage, the stop reason, the terminal states and
// the recovery diagnostics of this request.
func (f *runFinalizer) publish(stop string, err error) {
	usage := f.budget.report()
	if f.usage != nil {
		*f.usage = usage
	}
	// The pending set belongs to this request, so it is read and then emptied on
	// every exit path, including the one where there is no report to fill.
	open := f.conversation.OpenPendingCalls()
	f.conversation.forgetPendingCalls()
	if f.report == nil {
		return
	}
	if len(open) > 0 {
		ids := make([]string, 0, len(open))
		for _, entry := range open {
			ids = append(ids, entry.CallID)
		}
		f.report.PendingAtEnd = len(open)
		f.report.Warnings = append(f.report.Warnings, fmt.Sprintf("the request ended with %d tool call(s) still open: %s", len(open), strings.Join(ids, ", ")))
	}
	if stop == "" {
		stop = "unknown"
	}
	f.report.Iterations = f.iterations
	f.report.StopReason = stop
	if err != nil {
		f.report.Error = err.Error()
	}
	f.report.ModelCalls = usage.ModelCalls
	f.report.InputTokens = usage.InputTokens
	f.report.OutputTokens = usage.OutputTokens
	f.report.ReasoningTokens = usage.ReasoningTokens
	f.report.CachedInputTokens = usage.CachedInputTokens
	f.report.ExactUsage = usage.Exact
	f.report.EstimatedUsage = usage.Estimated
	f.report.RecoveredCalls = f.conversation.RecoveryNotes()
	// A host callback that failed is reported beside the outcome it did not
	// change, so "the request succeeded" and "the audit trail is incomplete" can
	// both be true and both be visible.
	if f.auditFailures != nil {
		f.report.AuditFailures = f.auditFailures()
	}
	if f.report.AuditFailures > 0 && f.auditDetail != nil {
		if detail := f.auditDetail(); detail != "" {
			f.report.Warnings = append(f.report.Warnings, fmt.Sprintf("%d host audit record(s) could not be recorded: %s", f.report.AuditFailures, detail))
		}
	}
	if f.events != nil {
		f.report.EventsDropped = f.events.droppedCount()
		if note := f.events.slowDiagnostic(); note != "" {
			f.report.Warnings = append(f.report.Warnings, note)
		}
	}
}

// countBatch records the terminal states of one executed batch: what ran, what
// was refused, what was cancelled and what stayed unknown.
func (f *runFinalizer) countBatch(states []protocol.CallState) {
	if f.report == nil {
		return
	}
	f.report.ToolCalls += len(states)
	for _, state := range states {
		f.report.count(state)
	}
}
