package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type Confirmation struct {
	Approved        bool
	RejectionReason string
}

type ConfirmFunc func(context.Context, policy.Request, policy.Outcome) (Confirmation, error)

// ApprovalError reports that the approval channel itself failed, as opposed to a
// user rejecting a request with or without a reason. An approval-channel failure
// is an infrastructure fault: it aborts the whole preflight batch, so calls that
// were already approved in the same batch do not run either, and the request
// reports a failure instead of a user interruption.
type ApprovalError struct {
	Tool string
	Err  error
}

func (e *ApprovalError) Error() string {
	return fmt.Sprintf("approval channel failed for %s: %v", e.Tool, e.Err)
}

func (e *ApprovalError) Unwrap() error { return e.Err }

// CheckpointSink records the lifecycle of side-effecting tool executions.
//
// The intent of a side-effecting call is persisted before the call starts, which
// is what shrinks the window in which an operation has happened but nothing
// records it. An intent that cannot be written prevents the call from running at
// all; a completion that cannot be written keeps the in-memory result, stops the
// batch from starting new side effects, and is reported distinctly.
type CheckpointSink interface {
	RecordIntent(Intent) error
	RecordCompletion(Completion) error
}

// BatchCheckpointSink lets a sink record the intents of one batch in a single
// durable write.
//
// The guarantee that matters is "no side effect starts before its intent is
// durable", and one write for the whole batch keeps it exactly: the write happens
// after every call of the batch is prepared and before any of them starts. Without
// it the executor falls back to one write per call, which is what every sink did
// before this interface existed, so a sink is never required to implement it.
//
// Completions are deliberately not batched. Merging them would save one write per
// call, but it would also delay the record of an operation that already happened
// past the point where the process could die, and for an effect with no file to
// inspect afterwards that turns a known result into an unknown one. Paying one
// write per call keeps the record immediate.
type BatchCheckpointSink interface {
	RecordIntents([]Intent) error
}

// Intent describes one execution that is about to start.
type Intent struct {
	RequestID    string
	CallID       string
	ParentCallID string
	Tool         string
	Risk         policy.Risk
	// TargetPath and PreviousHash are verifiable recovery hints: they help decide
	// whether the operation happened, and are never used to replay it.
	TargetPath   string
	PreviousHash string
}

// Completion describes the terminal outcome of one execution.
type Completion struct {
	RequestID  string
	CallID     string
	Tool       string
	State      protocol.CallState
	IsError    bool
	TargetPath string
	ResultHash string
}

// CheckpointError reports that the lifecycle of a call could not be recorded.
// Recorded distinguishes a failed intent, which prevents the call from running,
// from a failed completion, where the operation may already have happened.
type CheckpointError struct {
	CallID   string
	Recorded bool
	Err      error
}

func (e *CheckpointError) Error() string {
	if e.Recorded {
		return fmt.Sprintf("tool %s ran but its completion could not be recorded: %v", e.CallID, e.Err)
	}
	return fmt.Sprintf("tool %s was not started because its intent could not be recorded: %v", e.CallID, e.Err)
}

func (e *CheckpointError) Unwrap() error { return e.Err }

// joinBatchError keeps the first substantive failure and the request
// cancellation cause together. errors.Is then holds for either one, while the
// message still leads with the original failure. A batch-local cancel derived
// from a hook failure is never reported a second time.
func joinBatchError(primary, cancel error) error {
	switch {
	case primary == nil:
		return cancel
	case cancel == nil || errors.Is(primary, cancel):
		return primary
	default:
		return errors.Join(primary, cancel)
	}
}

type AuditRecord struct {
	Timestamp           time.Time           `json:"timestamp"`
	RequestID           string              `json:"request_id"`
	SessionID           string              `json:"session_id,omitempty"`
	ProviderName        string              `json:"provider_name,omitempty"`
	ProviderGeneration  uint64              `json:"provider_generation,omitempty"`
	Model               string              `json:"model,omitempty"`
	CallID              string              `json:"call_id"`
	ParentCallID        string              `json:"parent_call_id,omitempty"`
	BatchID             string              `json:"batch_id,omitempty"`
	BatchIndex          int                 `json:"batch_index"`
	Tool                string              `json:"tool"`
	Risk                policy.Risk         `json:"risk"`
	Decision            policy.Decision     `json:"decision"`
	Reason              string              `json:"reason"`
	Confirmed           bool                `json:"confirmed"`
	DurationMS          int64               `json:"duration_ms"`
	QueueDurationMS     int64               `json:"queue_duration_ms,omitempty"`
	ExecutionDurationMS int64               `json:"execution_duration_ms,omitempty"`
	ConcurrencyMode     string              `json:"concurrency_mode,omitempty"`
	ResourceClaims      []ResourceClaim     `json:"resource_claims,omitempty"`
	IsError             bool                `json:"is_error"`
	Truncated           bool                `json:"truncated"`
	InputBytes          int                 `json:"input_bytes"`
	OutputBytes         int                 `json:"output_bytes"`
	ExitCode            int                 `json:"exit_code,omitempty"`
	Mode                string              `json:"mode"`
	Classification      policy.CommandClass `json:"classification"`
	PolicyRule          string              `json:"policy_rule,omitempty"`
	PolicySource        string              `json:"policy_source,omitempty"`
	PolicyOverride      string              `json:"policy_override_ignored,omitempty"`
	Confirmations       int                 `json:"confirmations"`
	Warning             bool                `json:"warning"`
	SkillName           string              `json:"skill_name,omitempty"`
	SkillSource         string              `json:"skill_source,omitempty"`
	SkillDigest         string              `json:"skill_digest,omitempty"`
	SkillTrigger        string              `json:"skill_trigger,omitempty"`
	SkillActivated      string              `json:"skill_activated_at,omitempty"`
	AllowedTools        string              `json:"allowed_tools,omitempty"`
	SkillResource       string              `json:"skill_resource,omitempty"`
	ResourceBytes       int                 `json:"resource_bytes,omitempty"`
	WebBackend          string              `json:"web_backend,omitempty"`
	WebTarget           string              `json:"web_target,omitempty"`
	WebStatus           string              `json:"web_status,omitempty"`
	WebSources          int                 `json:"web_sources,omitempty"`
	WebInputTokens      int                 `json:"web_input_tokens,omitempty"`
	WebOutputTokens     int                 `json:"web_output_tokens,omitempty"`
	WebCostUSD          float64             `json:"web_cost_usd,omitempty"`
	UntrustedWebContent bool                `json:"untrusted_web_content,omitempty"`
}

type AuditSink interface {
	Record(AuditRecord)
}

type Executor struct {
	Registry  *Registry
	Policy    policy.Checker
	Confirm   ConfirmFunc
	Audit     AuditSink
	Workspace string
	// Timeout bounds one tool execution. It is a cooperative deadline, not a
	// hard preemption mechanism: the executor relies on the tool honouring
	// context cancellation and never starts an unreclaimable goroutine to fake a
	// hard timeout. Untrusted or non-cooperative plugins would need process
	// isolation, which is a separate enhancement.
	Timeout            time.Duration
	MaxOutputBytes     int
	SessionID          string
	ProviderName       string
	ProviderGeneration uint64
	Model              string
	MaxParallelTools   int
	Coordinator        *ResourceCoordinator
	// Checkpoint, when set, persists the lifecycle of side-effecting executions
	// around their execution.
	Checkpoint CheckpointSink
	// AuditDiagnostic, when set, receives one message the first time a host audit
	// record cannot be delivered. The failure is counted whether or not a
	// diagnostic is installed, so it never disappears silently.
	AuditDiagnostic func(string)
	// audit holds the per-executor audit counters. It is a pointer because an
	// Executor stays copyable: the app clones one to build the subagent executor.
	// A structural clone shares the counters, which is the same sharing its other
	// fields already have.
	audit *auditCounters
}

// auditCounters counts the host audit records one executor could not deliver.
//
// The counters are per executor, which is per request, because "the audit trail
// is incomplete" is a fact about one request rather than about the process.
type auditCounters struct {
	mu        sync.Mutex
	failures  int
	last      string
	diagnosed bool
}

// executorAuditInit guards the lazy allocation of Executor.audit. It is a package
// mutex rather than a struct field so that an Executor stays copyable: a copied
// pointer is fine, a copied lock is a bug.
var executorAuditInit sync.Mutex

func (e *Executor) auditCounter() *auditCounters {
	executorAuditInit.Lock()
	defer executorAuditInit.Unlock()
	if e.audit == nil {
		e.audit = &auditCounters{}
	}
	return e.audit
}

// AuditFailures reports how many host audit records could not be delivered.
func (e *Executor) AuditFailures() int {
	if e == nil {
		return 0
	}
	counters := e.auditCounter()
	counters.mu.Lock()
	defer counters.mu.Unlock()
	return counters.failures
}

// AuditFailureDetail describes the most recent audit failure, or an empty string.
func (e *Executor) AuditFailureDetail() string {
	if e == nil {
		return ""
	}
	counters := e.auditCounter()
	counters.mu.Lock()
	defer counters.mu.Unlock()
	return counters.last
}

// recordAudit hands one record to the host without letting the host kill the
// request.
//
// Auditing is an observation, not a step of the execution: a sink that fails or
// panics changes neither the call state nor the result, is never retried, and
// never blocks the batch. The contract is explicit because the alternative -
// letting a host callback end a request that already committed a side effect -
// loses the result of work that really happened. The failure is counted and
// diagnosed once, so it stays visible instead of disappearing.
func (e *Executor) recordAudit(record AuditRecord) {
	if e == nil || e.Audit == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			e.auditFailed(fmt.Sprintf("audit sink panicked: %v", recovered))
		}
	}()
	e.Audit.Record(record)
}

// auditFailed counts one undeliverable audit record and reports the first one.
func (e *Executor) auditFailed(reason string) {
	counters := e.auditCounter()
	counters.mu.Lock()
	counters.failures++
	counters.last = reason
	first := !counters.diagnosed
	counters.diagnosed = true
	counters.mu.Unlock()
	if first && e.AuditDiagnostic != nil {
		e.AuditDiagnostic(reason)
	}
}

type BatchHooks struct {
	OnStart  func(protocol.ToolCall) error
	OnResult func(protocol.ToolResult) error
}

type preparedCall struct {
	call             protocol.ToolCall
	item             Tool
	input            json.RawMessage
	outcome          policy.Outcome
	spec             ConcurrencySpec
	terminal         *protocol.ToolResult
	approvalErr      error
	state            protocol.CallState
	control          protocol.BatchControl
	requestID        string
	checkpointErr    error
	queuedAt         time.Time
	executionStarted time.Time
	record           AuditRecord
	auditOnce        sync.Once
	startNotified    bool
	running          bool
	done             bool
	// intentRecorded reports that the batch already wrote this call's intent, so
	// the per-call path in executePrepared must not write it a second time.
	intentRecorded bool
}

type batchCompletion struct {
	index  int
	result protocol.ToolResult
}

func (e *Executor) Definitions() []protocol.ToolDefinition {
	if e == nil || e.Registry == nil {
		return nil
	}
	definitions := e.Registry.Definitions()
	for index := range definitions {
		item, ok := e.Registry.Get(definitions[index].Name)
		if ok && item.Risk() != policy.RiskRead && item.Risk() != policy.RiskSession {
			definitions[index] = withApprovalReason(definitions[index])
		}
	}
	return definitions
}

func (e *Executor) CanExecuteConcurrently(call protocol.ToolCall) bool {
	if e == nil || e.Registry == nil {
		return false
	}
	item, ok := e.Registry.Get(call.Name)
	if !ok {
		return false
	}
	if classifier, ok := item.(ConcurrencyClassifier); ok {
		return normalizeConcurrencySpec(classifier.ClassifyConcurrency(call.Arguments, policy.Outcome{})).Mode != ConcurrencyExclusive
	}
	safe, ok := item.(ParallelSafe)
	return ok && safe.ParallelSafe()
}

func (e *Executor) ExecuteConcurrent(ctx context.Context, requestID string, calls []protocol.ToolCall) []protocol.ToolResult {
	results, _ := e.ExecuteBatch(ctx, requestID, calls, BatchHooks{})
	return results
}

func (e *Executor) ParallelLimit() int {
	if e == nil || e.MaxParallelTools <= 0 {
		return 4
	}
	return e.MaxParallelTools
}

func (e *Executor) Execute(ctx context.Context, requestID string, call protocol.ToolCall) protocol.ToolResult {
	prepared := e.prepareCall(ctx, requestID, "", 0, call, time.Now())
	if prepared.terminal != nil {
		return e.finishPrepared(prepared, *prepared.terminal, 0)
	}
	return e.executePrepared(ctx, prepared)
}

func (e *Executor) ExecuteBatch(ctx context.Context, requestID string, calls []protocol.ToolCall, hooks BatchHooks) ([]protocol.ToolResult, error) {
	results, outcome := e.ExecuteBatchOutcome(ctx, requestID, calls, hooks, e.ParallelLimit())
	return results, outcome.Err()
}

func (e *Executor) ExecuteBatchWithLimit(ctx context.Context, requestID string, calls []protocol.ToolCall, hooks BatchHooks, limit int) ([]protocol.ToolResult, error) {
	results, outcome := e.ExecuteBatchOutcome(ctx, requestID, calls, hooks, limit)
	return results, outcome.Err()
}

// ExecuteBatchOutcome runs a batch and also returns its request-level control
// state. Callers that decide the fate of a request must use this form: the
// control state is owned by the executor and never derived from tool metadata.
func (e *Executor) ExecuteBatchOutcome(ctx context.Context, requestID string, calls []protocol.ToolCall, hooks BatchHooks, limit int) ([]protocol.ToolResult, protocol.BatchOutcome) {
	if limit <= 0 {
		limit = e.ParallelLimit()
	}
	return e.executeBatch(ctx, requestID, calls, hooks, limit)
}

// resolveBatchControl maps the failures observed by one batch to its
// request-level control state.
//
// batchErr only ever holds an approval-channel, hook, checkpoint or scheduler
// failure: an ordinary tool failure never reaches it, so the model keeps the
// chance to adjust. The request cancellation is a separate cause and is
// preserved beside an infrastructure failure.
//
// The order below is the priority between competing stop reasons, and it is
// deliberately fixed: an infrastructure failure leads because a batch that
// cannot record what it does must not keep working whatever else happened; a
// request cancellation leads a user interruption because it is the wider stop.
// The interruption is never lost when it loses here - it stays on the call
// states and on the not-executed results, so "the user stopped this" is still
// readable from the outcome.
func resolveBatchControl(ctx context.Context, batchErr error, interrupted bool) (protocol.BatchControl, error) {
	if batchErr != nil {
		return protocol.ControlAbortRequest, joinBatchError(batchErr, ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return protocol.ControlCancelRequest, err
	}
	if interrupted {
		return protocol.ControlInterruptRequest, nil
	}
	return protocol.ControlContinue, nil
}

// batchStopped reports whether the batch may still start the next call.
//
// It is the single answer to "may another call begin", and it is deliberately
// derived from the same three observations resolveBatchControl reports: an
// accepted user interruption, a request cancellation, and an infrastructure
// failure. Every place that decides to start a call asks this one function, so
// a control state that says "stopped" can never coexist with a call that
// started after it.
func batchStopped(ctx context.Context, batchErr error, interrupted bool) bool {
	return interrupted || batchErr != nil || ctx.Err() != nil
}

// notStartedResult builds the terminal result of a call that was prepared but
// never allowed to start. It says why no work happened and never claims
// success; the interruption flag is carried explicitly so a reader does not
// have to infer "the user stopped this" from the message text.
func notStartedResult(call protocol.ToolCall, message string, interrupted bool) protocol.ToolResult {
	metadata := map[string]any{"batch_cancelled": true}
	if interrupted {
		metadata["interrupt_request"] = true
	}
	return protocol.ToolResult{CallID: call.ID, Content: message, IsError: true, Metadata: metadata}
}

// notStartedMessage names the reason a prepared call was never allowed to start.
func notStartedMessage(batchErr error, interrupted bool) string {
	switch {
	case batchErr != nil:
		return "tool execution cancelled"
	case interrupted:
		return "tool execution cancelled by user interruption"
	default:
		return "tool execution cancelled by batch preflight"
	}
}

func (e *Executor) executeBatch(ctx context.Context, requestID string, calls []protocol.ToolCall, hooks BatchHooks, limit int) ([]protocol.ToolResult, protocol.BatchOutcome) {
	results := make([]protocol.ToolResult, len(calls))
	if len(calls) == 0 {
		return results, protocol.BatchOutcome{Control: protocol.ControlContinue}
	}
	batchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	queuedAt := time.Now()
	batchID := uuid.NewString()
	prepared := make([]*preparedCall, len(calls))
	interrupted := false
	// batchErr holds an approval-channel, hook or scheduler failure. Ordinary
	// tool failures never reach it, so the model keeps the chance to adjust.
	var batchErr error
	// finalize reports the control state the batch observed even when every call
	// already produced a result. The results themselves are kept: once a call
	// committed a side effect, its outcome is never replaced.
	finalize := func() ([]protocol.ToolResult, protocol.BatchOutcome) {
		control, cause := resolveBatchControl(ctx, batchErr, interrupted)
		states := make([]protocol.CallState, len(prepared))
		for index, item := range prepared {
			if item != nil {
				states[index] = item.state
			}
		}
		return results, protocol.BatchOutcome{Control: control, Cause: cause, States: states}
	}
	for index, call := range calls {
		// Observe a request cancellation before preparing the next call, so a
		// cancelled request never reaches the approval or execution stage.
		batchErr = joinBatchError(batchErr, ctx.Err())
		if interrupted || batchErr != nil {
			prepared[index] = e.cancelledPrepared(requestID, batchID, index, call, queuedAt, notStartedMessage(batchErr, interrupted), interrupted)
			continue
		}
		prepared[index] = e.prepareCall(batchCtx, requestID, batchID, index, call, queuedAt)
		// An approval-channel failure is infrastructure, not a user decision, so
		// it aborts the preflight batch instead of letting the remaining calls
		// run.
		if prepared[index].approvalErr != nil {
			batchErr = joinBatchError(batchErr, prepared[index].approvalErr)
		}
		if prepared[index].control == protocol.ControlInterruptRequest {
			interrupted = true
		}
	}
	if interrupted || batchErr != nil {
		message := notStartedMessage(batchErr, interrupted)
		for index, item := range prepared {
			if item == nil {
				prepared[index] = e.cancelledPrepared(requestID, batchID, index, calls[index], queuedAt, message, interrupted)
			} else if item.terminal == nil {
				result := notStartedResult(item.call, message, interrupted)
				item.terminal = &result
				item.state = protocol.CallNotExecuted
			}
		}
	}

	completed := 0
	for index, item := range prepared {
		if item.terminal == nil {
			continue
		}
		result, hookErr := e.deliverTerminal(item, *item.terminal, hooks)
		results[index] = result
		item.done = true
		completed++
		if hookErr != nil {
			batchErr = joinBatchError(batchErr, hookErr)
			cancel()
			hooks = BatchHooks{}
		}
	}
	// The intents of every call that may still run are written together, before
	// any of them can start. That is the whole point of the batch write: N
	// side-effecting calls cost one append instead of N, and the guarantee
	// "the intent is durable before the side effect" is untouched because the
	// write still completes before the first call starts.
	if err := e.recordBatchIntents(batchCtx, prepared); err != nil {
		batchErr = joinBatchError(batchErr, err)
		cancel()
		hooks = BatchHooks{}
	}
	if completed == len(prepared) {
		return finalize()
	}

	if limit > len(prepared) {
		limit = len(prepared)
	}
	active := make(map[int]*preparedCall)
	completion := make(chan batchCompletion, len(prepared))
	contextDone := batchCtx.Done()
	for completed < len(prepared) {
		if !batchStopped(batchCtx, batchErr, interrupted) {
			for len(active) < limit {
				// A stop observed at any point before this call starts
				// must prevent the call from starting. The stop reason reaches
				// the caller through resolveBatchControl.
				if batchStopped(batchCtx, batchErr, interrupted) {
					break
				}
				index := nextRunnable(prepared, active)
				if index < 0 {
					break
				}
				item := prepared[index]
				if hooks.OnStart != nil {
					if err := hooks.OnStart(item.call); err != nil {
						batchErr = joinBatchError(batchErr, err)
						cancel()
						hooks = BatchHooks{}
						break
					}
					item.startNotified = true
				}
				// OnStart may itself observe or trigger a stop, so the check is
				// repeated before the call is actually started. The
				// cancellation is reported by resolveBatchControl rather than
				// being folded into the infrastructure failure.
				if batchStopped(batchCtx, batchErr, interrupted) {
					break
				}
				item.running = true
				active[index] = item
				go func(index int, item *preparedCall) {
					completion <- batchCompletion{index: index, result: e.executePrepared(batchCtx, item)}
				}(index, item)
				if item.spec.Mode == ConcurrencyExclusive {
					break
				}
			}
		}
		if batchStopped(batchCtx, batchErr, interrupted) {
			// Close the calls that never started. An interruption accepted while
			// the batch was running reaches this point with the request context
			// still alive, so the drain must be driven by the stop predicate and
			// not by the cancellation alone. The stop reason itself is reported
			// by resolveBatchControl, which owns the request-level control state;
			// a batch-local cancel derived from a hook failure is deliberately
			// not reported again, so the original failure stays the leading
			// cause.
			message := notStartedMessage(batchErr, interrupted)
			for index, item := range prepared {
				if item.done || item.running {
					continue
				}
				item.state = protocol.CallNotExecuted
				result, hookErr := e.deliverTerminal(item, notStartedResult(item.call, message, interrupted), hooks)
				results[index] = result
				item.done = true
				completed++
				if hookErr != nil {
					batchErr = joinBatchError(batchErr, hookErr)
					hooks = BatchHooks{}
				}
			}
			contextDone = nil
		}
		if completed == len(prepared) {
			break
		}
		if len(active) == 0 {
			if !batchStopped(batchCtx, batchErr, interrupted) {
				batchErr = fmt.Errorf("tool scheduler could not make progress")
				cancel()
				continue
			}
			continue
		}
		select {
		case finished := <-completion:
			item := prepared[finished.index]
			delete(active, finished.index)
			item.running = false
			item.done = true
			results[finished.index] = finished.result
			completed++
			// A host tool may have requested a user interruption while it ran.
			if item.control == protocol.ControlInterruptRequest {
				interrupted = true
			}
			// A lifecycle record that could not be written stops the batch: the
			// operation may have happened without a record, so no further side
			// effect may start.
			if item.checkpointErr != nil {
				batchErr = joinBatchError(batchErr, item.checkpointErr)
				cancel()
				hooks = BatchHooks{}
			}
			if hooks.OnResult != nil {
				if err := hooks.OnResult(finished.result); err != nil {
					batchErr = joinBatchError(batchErr, err)
					cancel()
					hooks = BatchHooks{}
				}
			}
		case <-contextDone:
			// The cancellation is picked up by the next iteration and reported
			// once by resolveBatchControl.
			contextDone = nil
		}
	}
	return finalize()
}

func (e *Executor) prepareCall(ctx context.Context, requestID, batchID string, batchIndex int, call protocol.ToolCall, queuedAt time.Time) (prepared *preparedCall) {
	prepared = &preparedCall{call: call, queuedAt: queuedAt, spec: ConcurrencySpec{Mode: ConcurrencyExclusive}}
	prepared.record = AuditRecord{Timestamp: time.Now().UTC(), RequestID: requestID, BatchID: batchID, BatchIndex: batchIndex, CallID: call.ID, ParentCallID: call.ParentCallID, Tool: call.Name, InputBytes: len(call.Arguments), ConcurrencyMode: string(ConcurrencyExclusive)}
	if e != nil {
		prepared.record.SessionID, prepared.record.ProviderName = e.SessionID, e.ProviderName
		prepared.record.ProviderGeneration, prepared.record.Model = e.ProviderGeneration, e.Model
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			result := protocol.ToolResult{CallID: call.ID, Content: fmt.Sprintf("tool preparation panicked: %v", recovered), IsError: true}
			prepared.terminal = &result
		}
	}()
	if e == nil || e.Registry == nil {
		result := protocol.ToolResult{CallID: call.ID, Content: "tool executor is unavailable", IsError: true}
		prepared.terminal, prepared.state = &result, protocol.CallFailed
		return prepared
	}
	if ctx.Err() != nil {
		result := protocol.ToolResult{CallID: call.ID, Content: "tool execution cancelled", IsError: true}
		prepared.terminal, prepared.state = &result, protocol.CallNotExecuted
		return prepared
	}
	item, ok := e.Registry.Get(call.Name)
	if !ok {
		result := protocol.ToolResult{CallID: call.ID, Content: fmt.Sprintf("unknown tool %q", call.Name), IsError: true}
		prepared.terminal, prepared.state = &result, protocol.CallFailed
		return prepared
	}
	prepared.item = item
	if !json.Valid(call.Arguments) {
		result := protocol.ToolResult{CallID: call.ID, Content: "tool input is invalid JSON", IsError: true}
		prepared.terminal, prepared.state = &result, protocol.CallFailed
		return prepared
	}
	checker := e.Policy
	if checker == nil {
		checker = policy.BaselineChecker{}
	}
	policyRequest := policy.Request{Tool: call.Name, Input: call.Arguments, Workspace: e.Workspace, Risk: item.Risk()}
	var override *policy.Outcome
	if toolOverride, ok := item.(PolicyOverride); ok {
		if domainOutcome, applied := toolOverride.OverridePolicy(call.Arguments); applied {
			override = &domainOutcome
		}
	}
	// The workspace layer keeps a non-relaxable prohibition, the mode and the
	// risk; a tool-level policy may only decide inside its own granted domain.
	// An unrecognized decision fails closed instead of executing.
	outcome := policy.Compose(checker.Check(ctx, policyRequest), override)
	prepared.outcome = outcome
	prepared.record.Risk, prepared.record.Decision, prepared.record.Reason = outcome.Risk, outcome.Decision, outcome.Reason
	prepared.record.Mode, prepared.record.Classification, prepared.record.Warning = outcome.Mode.String(), outcome.Classification, outcome.Warning
	prepared.record.PolicyRule, prepared.record.PolicySource, prepared.record.PolicyOverride = outcome.Rule, string(outcome.Source), outcome.Override
	// The request ID is carried into the lifecycle records, so recovery can relate
	// an intent to the request it belonged to.
	prepared.requestID = requestID
	switch outcome.Decision {
	case policy.DecisionAllow:
	case policy.DecisionDeny:
		result := protocol.ToolResult{CallID: call.ID, Content: "permission denied: " + outcome.Reason, IsError: true}
		prepared.terminal, prepared.state = &result, protocol.CallRejected
		return prepared
	case policy.DecisionConfirm:
		if e.Confirm == nil {
			result := protocol.ToolResult{CallID: call.ID, Content: "confirmation required: " + outcome.Reason, IsError: true}
			prepared.terminal, prepared.state = &result, protocol.CallFailed
			return prepared
		}
		confirmations := outcome.Confirmations
		if confirmations <= 0 {
			confirmations = 1
		}
		for step := 1; step <= confirmations; step++ {
			policyRequest.ConfirmationStep = step
			policyRequest.ConfirmationTotal = confirmations
			confirmation, err := e.Confirm(ctx, policyRequest, outcome)
			if err != nil {
				prepared.approvalErr = &ApprovalError{Tool: call.Name, Err: err}
				result := protocol.ToolResult{CallID: call.ID, Content: "confirmation failed: " + err.Error(), IsError: true}
				prepared.terminal, prepared.state = &result, protocol.CallFailed
				return prepared
			}
			if !confirmation.Approved {
				result := protocol.ToolResult{CallID: call.ID, Content: "approval rejected", IsError: true, Metadata: map[string]any{"approval_rejected": true}}
				prepared.state = protocol.CallRejected
				if reason := strings.TrimSpace(confirmation.RejectionReason); reason != "" {
					// A reasoned refusal keeps the reason and lets the model
					// adjust, so it does not interrupt the request.
					result.Content += ": " + reason
					result.Metadata["rejection_reason"] = reason
				} else {
					// An unexplained refusal is a user interruption request.
					result.Metadata["interrupt_request"] = true
					prepared.control = protocol.ControlInterruptRequest
				}
				prepared.terminal = &result
				return prepared
			}
			prepared.record.Confirmations++
		}
		prepared.record.Confirmed = true
	default:
		// Defense in depth: Compose already fails closed, but an unrecognized
		// decision must never reach a tool execution.
		result := protocol.ToolResult{CallID: call.ID, Content: "permission denied: unrecognized policy decision", IsError: true}
		prepared.terminal, prepared.state = &result, protocol.CallRejected
		return prepared
	}
	prepared.input = call.Arguments
	if !schemaHasProperty(item.Definition().InputSchema, "reason") {
		prepared.input = withoutJSONField(prepared.input, "reason")
	}
	prepared.spec = concurrencySpec(item, prepared.input, outcome)
	prepared.record.ConcurrencyMode = string(prepared.spec.Mode)
	prepared.record.ResourceClaims = append([]ResourceClaim(nil), prepared.spec.Claims...)
	return prepared
}

// cancelledExecution reports a call that was cancelled before the tool started.
// The batch already reports the request-level cancellation, so the result only
// has to be recognizable and must never claim success.
func cancelledExecution(prepared *preparedCall, err error) protocol.ToolResult {
	prepared.state = protocol.CallNotExecuted
	message := "tool execution cancelled"
	if err != nil {
		message += ": " + err.Error()
	}
	return protocol.ToolResult{CallID: prepared.call.ID, Content: message, IsError: true}
}

func (e *Executor) cancelledPrepared(requestID, batchID string, batchIndex int, call protocol.ToolCall, queuedAt time.Time, message string, interrupted bool) *preparedCall {
	prepared := &preparedCall{call: call, queuedAt: queuedAt, spec: ConcurrencySpec{Mode: ConcurrencyExclusive}, state: protocol.CallNotExecuted}
	prepared.record = AuditRecord{Timestamp: time.Now().UTC(), RequestID: requestID, BatchID: batchID, BatchIndex: batchIndex, CallID: call.ID, ParentCallID: call.ParentCallID, Tool: call.Name, InputBytes: len(call.Arguments), ConcurrencyMode: string(ConcurrencyExclusive)}
	if e != nil {
		prepared.record.SessionID, prepared.record.ProviderName = e.SessionID, e.ProviderName
		prepared.record.ProviderGeneration, prepared.record.Model = e.ProviderGeneration, e.Model
	}
	result := notStartedResult(call, message, interrupted)
	prepared.terminal = &result
	return prepared
}

func (e *Executor) executePrepared(ctx context.Context, prepared *preparedCall) (result protocol.ToolResult) {
	var executionStarted time.Time
	defer func() {
		if recovered := recover(); recovered != nil {
			result = protocol.ToolResult{CallID: prepared.call.ID, Content: fmt.Sprintf("tool execution panicked: %v", recovered), IsError: true}
			prepared.state = protocol.CallFailed
		}
		result.CallID = prepared.call.ID
		duration := time.Duration(0)
		if !executionStarted.IsZero() {
			duration = time.Since(executionStarted)
		}
		result = e.finishPrepared(prepared, result, duration)
	}()
	if ctx.Err() != nil {
		return cancelledExecution(prepared, ctx.Err())
	}
	release, err := e.Coordinator.Acquire(ctx, prepared.spec)
	if err != nil {
		return cancelledExecution(prepared, err)
	}
	defer release()
	// Re-check after the resource was granted: a cancellation observed while
	// waiting for the claim must not start a new side effect.
	if err := ctx.Err(); err != nil {
		return cancelledExecution(prepared, err)
	}
	if finalizer, ok := prepared.item.(ExecutionFinalizer); ok {
		defer finalizer.AfterExecute(prepared.outcome)
	}
	// A side-effecting call records its intent before it runs. When that record
	// cannot be written the call does not start: an operation the host cannot log
	// must not happen. A batch that already wrote every intent in one append owns
	// this, so the call is only skipped, not recorded twice.
	intent := e.executionIntent(prepared)
	if e.Checkpoint != nil && intent != nil && !prepared.intentRecorded {
		if err := e.Checkpoint.RecordIntent(*intent); err != nil {
			prepared.state = protocol.CallNotExecuted
			prepared.checkpointErr = &CheckpointError{CallID: prepared.call.ID, Err: err}
			return protocol.ToolResult{
				CallID: prepared.call.ID, IsError: true, State: protocol.CallNotExecuted,
				Content: "tool call was not executed: " + (&CheckpointError{CallID: prepared.call.ID, Err: err}).Error(),
			}
		}
	}
	prepared.executionStarted = time.Now()
	executionStarted = prepared.executionStarted
	toolCtx := ctx
	cancel := func() {}
	useTimeout := true
	if timeoutPolicy, ok := prepared.item.(ExecutorTimeoutPolicy); ok {
		useTimeout = timeoutPolicy.UseExecutorTimeout()
	}
	if useTimeout {
		timeout := e.Timeout
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		toolCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	result = prepared.item.Execute(toolCtx, prepared.input)
	// A result that reports success is never rewritten as cancelled or timed
	// out: the tool may have committed a side effect before the cancellation was
	// observed, and a committed effect must never be reported as if it had not
	// run. The request-level cancellation is still reported by the batch.
	if result.IsError {
		switch {
		case useTimeout && toolCtx.Err() == context.DeadlineExceeded:
			result.Content = "tool execution timed out"
			prepared.state = protocol.CallFailed
		case toolCtx.Err() == context.Canceled:
			result.Content = "tool execution cancelled"
			prepared.state = protocol.CallCancelled
		default:
			prepared.state = protocol.CallFailed
		}
	} else {
		prepared.state = protocol.CallSucceeded
	}
	// Only host-registered tools can raise a request-level control state from
	// their result. Untrusted tool content and metadata cannot.
	if reporter, ok := prepared.item.(ControlReporter); ok {
		control, state := reporter.ReportControl(result)
		if control != "" {
			prepared.control = control
		}
		if state != "" {
			prepared.state = state
		}
	}
	maxOutput := e.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = 64 << 10
	}
	content, truncated := truncateUTF8(result.Content, maxOutput)
	result.Content = content
	result.Truncated = result.Truncated || truncated
	// The completion is recorded as soon as the operation is over. A failure here
	// keeps the in-memory result, stops the batch from starting new side effects,
	// and says explicitly that the operation may have happened.
	if e.Checkpoint != nil && intent != nil {
		completion := Completion{
			RequestID: prepared.requestID, CallID: prepared.call.ID, Tool: prepared.call.Name,
			State: prepared.state, IsError: result.IsError, TargetPath: intent.TargetPath, ResultHash: resultHash(result),
		}
		if err := e.Checkpoint.RecordCompletion(completion); err != nil {
			prepared.checkpointErr = &CheckpointError{CallID: prepared.call.ID, Recorded: true, Err: err}
			if result.Metadata == nil {
				result.Metadata = make(map[string]any, 1)
			}
			result.Metadata["checkpoint_incomplete"] = true
		}
	}
	return result
}

// recordBatchIntents writes the intents of every call that may still run in one
// durable write, and closes the calls whose intent could not be written.
//
// A sink that does not implement BatchCheckpointSink is left alone: the per-call
// path in executePrepared writes its own intent immediately before its call, which
// is the same guarantee at a higher cost.

func (e *Executor) recordBatchIntents(ctx context.Context, prepared []*preparedCall) error {
	if e == nil || e.Checkpoint == nil {
		return nil
	}
	batch, ok := e.Checkpoint.(BatchCheckpointSink)
	if !ok {
		return nil
	}
	intents := make([]Intent, 0, len(prepared))
	owners := make([]*preparedCall, 0, len(prepared))
	for _, item := range prepared {
		// A call that already reached a terminal state or was cancelled by the
		// preflight must not be given an intent: the intent is evidence that the
		// operation may happen, and this one will not.
		if item == nil || item.terminal != nil || item.running || item.done {
			continue
		}
		if ctx.Err() != nil {
			continue
		}
		intent := e.executionIntent(item)
		if intent == nil {
			continue
		}
		intents = append(intents, *intent)
		owners = append(owners, item)
	}
	if len(intents) == 0 {
		return nil
	}
	if err := batch.RecordIntents(intents); err != nil {
		for _, item := range owners {
			item.state = protocol.CallNotExecuted
			item.checkpointErr = &CheckpointError{CallID: item.call.ID, Err: err}
			result := protocol.ToolResult{
				CallID: item.call.ID, IsError: true, State: protocol.CallNotExecuted,
				Content: "tool call was not executed: " + (&CheckpointError{CallID: item.call.ID, Err: err}).Error(),
			}
			item.terminal = &result
		}
		return &CheckpointError{CallID: owners[0].call.ID, Err: err}
	}
	for _, item := range owners {
		item.intentRecorded = true
	}
	return nil
}

// executionIntent builds the lifecycle record of one side-effecting call. A call
// that only reads anything, or that is already a terminal result, needs none.
func (e *Executor) executionIntent(prepared *preparedCall) *Intent {
	if e.Checkpoint == nil || prepared.item == nil {
		return nil
	}
	switch prepared.outcome.Risk {
	case policy.RiskRead, policy.RiskSession:
		return nil
	}
	intent := &Intent{
		RequestID: prepared.requestID, CallID: prepared.call.ID, ParentCallID: prepared.call.ParentCallID,
		Tool: prepared.call.Name, Risk: prepared.outcome.Risk,
	}
	// A tool that can describe its target without running gives the verifiable
	// recovery hints; otherwise the path from the call arguments is used.
	if reporter, ok := prepared.item.(IntentReporter); ok {
		intent.TargetPath, intent.PreviousHash = reporter.ReportIntent(prepared.input)
		return intent
	}
	var fields struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(prepared.input, &fields) == nil {
		intent.TargetPath = fields.Path
	}
	return intent
}

// resultHash returns the content hash a tool reported for its target, when it has
// one. It is a recovery hint, not a guarantee.
func resultHash(result protocol.ToolResult) string {
	value, _ := result.Metadata["file_hash"].(string)
	return value
}

func (e *Executor) finishPrepared(prepared *preparedCall, result protocol.ToolResult, executionDuration time.Duration) protocol.ToolResult {
	result.CallID = prepared.call.ID
	// Every terminal result carries a host-owned state, so a caller can always
	// tell "not executed" from "failed" from "succeeded".
	if prepared.state == "" {
		if result.IsError {
			prepared.state = protocol.CallFailed
		} else {
			prepared.state = protocol.CallSucceeded
		}
	}
	result.State = prepared.state
	prepared.auditOnce.Do(func() {
		prepared.record.DurationMS = time.Since(prepared.queuedAt).Milliseconds()
		if !prepared.executionStarted.IsZero() {
			prepared.record.QueueDurationMS = prepared.executionStarted.Sub(prepared.queuedAt).Milliseconds()
		} else {
			prepared.record.QueueDurationMS = prepared.record.DurationMS
		}
		prepared.record.ExecutionDurationMS = executionDuration.Milliseconds()
		prepared.record.IsError = result.IsError
		prepared.record.Truncated = result.Truncated
		prepared.record.OutputBytes = len([]byte(result.Content))
		if result.Metadata != nil {
			if exitCode, ok := result.Metadata["exit_code"].(int); ok {
				prepared.record.ExitCode = exitCode
			}
			prepared.record.SkillName, _ = result.Metadata["skill_name"].(string)
			prepared.record.SkillSource, _ = result.Metadata["skill_source"].(string)
			prepared.record.SkillDigest, _ = result.Metadata["skill_digest"].(string)
			prepared.record.SkillTrigger, _ = result.Metadata["trigger"].(string)
			prepared.record.SkillActivated, _ = result.Metadata["activated_at"].(string)
			prepared.record.AllowedTools, _ = result.Metadata["allowed_tools"].(string)
			prepared.record.SkillResource, _ = result.Metadata["resource"].(string)
			prepared.record.ResourceBytes, _ = result.Metadata["bytes"].(int)
			prepared.record.WebBackend, _ = result.Metadata["web_backend"].(string)
			prepared.record.WebTarget, _ = result.Metadata["web_target"].(string)
			prepared.record.WebStatus, _ = result.Metadata["web_status"].(string)
			prepared.record.WebSources, _ = result.Metadata["citation_count"].(int)
			prepared.record.WebInputTokens, _ = result.Metadata["web_input_tokens"].(int)
			prepared.record.WebOutputTokens, _ = result.Metadata["web_output_tokens"].(int)
			prepared.record.WebCostUSD, _ = result.Metadata["web_cost_usd"].(float64)
			prepared.record.UntrustedWebContent, _ = result.Metadata["untrusted_web_content"].(bool)
		}
		if e != nil && e.Audit != nil {
			// A host audit failure is isolated here: it changes neither the call
			// state nor the result, and it never ends the request.
			e.recordAudit(prepared.record)
		}
	})
	return result
}

// deliverTerminal emits the start event for a call that never reached the
// scheduler and then reports its terminal result. A call that already emitted
// its start event is not announced twice.
func (e *Executor) deliverTerminal(prepared *preparedCall, result protocol.ToolResult, hooks BatchHooks) (protocol.ToolResult, error) {
	if hooks.OnStart != nil && !prepared.startNotified {
		if err := hooks.OnStart(prepared.call); err != nil {
			return e.finishPrepared(prepared, result, 0), err
		}
		prepared.startNotified = true
	}
	result = e.finishPrepared(prepared, result, 0)
	if hooks.OnResult != nil {
		if err := hooks.OnResult(result); err != nil {
			return result, err
		}
	}
	return result, nil
}

func concurrencySpec(item Tool, input json.RawMessage, outcome policy.Outcome) ConcurrencySpec {
	if classifier, ok := item.(ConcurrencyClassifier); ok {
		return normalizeConcurrencySpec(classifier.ClassifyConcurrency(input, outcome))
	}
	if safe, ok := item.(ParallelSafe); ok && safe.ParallelSafe() {
		return ConcurrencySpec{Mode: ConcurrencyShared}
	}
	return ConcurrencySpec{Mode: ConcurrencyExclusive}
}

func normalizeConcurrencySpec(spec ConcurrencySpec) ConcurrencySpec {
	switch spec.Mode {
	case ConcurrencyShared:
		return ConcurrencySpec{Mode: ConcurrencyShared}
	case ConcurrencyClaimed:
		claims := make([]ResourceClaim, 0, len(spec.Claims))
		for _, claim := range spec.Claims {
			// Claims are canonicalized through the same entry point as the tools,
			// so aliases of one path always compare as one resource.
			claim.Path = resourceKey(claim.Path)
			if claim.Path == "" || claim.Kind != ResourceFile && claim.Kind != ResourceTree || claim.Access != ResourceRead && claim.Access != ResourceWrite {
				return ConcurrencySpec{Mode: ConcurrencyExclusive}
			}
			claims = append(claims, claim)
		}
		if len(claims) == 0 {
			return ConcurrencySpec{Mode: ConcurrencyExclusive}
		}
		return ConcurrencySpec{Mode: ConcurrencyClaimed, Claims: claims}
	default:
		return ConcurrencySpec{Mode: ConcurrencyExclusive}
	}
}

func nextRunnable(prepared []*preparedCall, active map[int]*preparedCall) int {
	for index, item := range prepared {
		if item.done || item.running || item.terminal != nil {
			continue
		}
		if canStartCall(prepared, active, index) {
			return index
		}
	}
	return -1
}

func canStartCall(prepared []*preparedCall, active map[int]*preparedCall, index int) bool {
	item := prepared[index]
	if item.spec.Mode == ConcurrencyExclusive {
		if len(active) > 0 {
			return false
		}
		for earlier := 0; earlier < index; earlier++ {
			if !prepared[earlier].done {
				return false
			}
		}
		return true
	}
	for earlier := 0; earlier < index; earlier++ {
		other := prepared[earlier]
		if other.done {
			continue
		}
		if other.spec.Mode == ConcurrencyExclusive || concurrencyConflicts(item.spec, other.spec) {
			return false
		}
	}
	for _, other := range active {
		if other.spec.Mode == ConcurrencyExclusive || concurrencyConflicts(item.spec, other.spec) {
			return false
		}
	}
	return true
}

func concurrencyConflicts(left, right ConcurrencySpec) bool {
	if left.Mode == ConcurrencyExclusive || right.Mode == ConcurrencyExclusive {
		return true
	}
	if left.Mode != ConcurrencyClaimed || right.Mode != ConcurrencyClaimed {
		return false
	}
	for _, first := range left.Claims {
		for _, second := range right.Claims {
			if first.Access == ResourceRead && second.Access == ResourceRead {
				continue
			}
			if resourceClaimsOverlap(first, second) {
				return true
			}
		}
	}
	return false
}

func resourceClaimsOverlap(left, right ResourceClaim) bool {
	switch {
	case left.Kind == ResourceFile && right.Kind == ResourceFile:
		return left.Path == right.Path
	case left.Kind == ResourceTree && right.Kind == ResourceFile:
		return resourceTreeContains(left.Path, right.Path)
	case left.Kind == ResourceFile && right.Kind == ResourceTree:
		return resourceTreeContains(right.Path, left.Path)
	default:
		return resourceTreeContains(left.Path, right.Path) || resourceTreeContains(right.Path, left.Path)
	}
}

func resourceTreeContains(tree, candidate string) bool {
	if tree == "" || candidate == "" {
		return false
	}
	prefix := tree
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return candidate == tree || strings.HasPrefix(candidate, prefix)
}

func withApprovalReason(definition protocol.ToolDefinition) protocol.ToolDefinition {
	if schemaHasProperty(definition.InputSchema, "reason") {
		return definition
	}
	var schema map[string]any
	if json.Unmarshal(definition.InputSchema, &schema) != nil || schema["type"] != "object" {
		return definition
	}
	properties, _ := schema["properties"].(map[string]any)
	if properties == nil {
		properties = make(map[string]any)
		schema["properties"] = properties
	}
	properties["reason"] = map[string]any{"type": "string", "minLength": 1, "description": "User-facing reason"}
	required, _ := schema["required"].([]any)
	schema["required"] = append(required, "reason")
	encoded, err := json.Marshal(schema)
	if err == nil {
		definition.InputSchema = encoded
	}
	return definition
}

func schemaHasProperty(schema json.RawMessage, name string) bool {
	var decoded struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(schema, &decoded) != nil {
		return false
	}
	_, exists := decoded.Properties[name]
	return exists
}

func withoutJSONField(input json.RawMessage, name string) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(input, &fields) != nil {
		return input
	}
	if _, exists := fields[name]; !exists {
		return input
	}
	delete(fields, name)
	encoded, err := json.Marshal(fields)
	if err != nil {
		return input
	}
	return encoded
}

func truncateUTF8(value string, limit int) (string, bool) {
	if limit <= 0 || len(value) <= limit {
		return value, false
	}
	marker := "\n[output truncated]"
	if limit <= len(marker) {
		return marker[:limit], true
	}
	end := limit - len(marker)
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + marker, true
}
