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
	queuedAt         time.Time
	executionStarted time.Time
	record           AuditRecord
	auditOnce        sync.Once
	startNotified    bool
	running          bool
	done             bool
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
// batchErr only ever holds an approval-channel, hook or scheduler failure: an
// ordinary tool failure never reaches it, so the model keeps the chance to
// adjust. The request cancellation is a separate cause and is preserved beside
// an infrastructure failure.
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
			prepared[index] = e.cancelledPrepared(requestID, batchID, index, call, queuedAt, "tool execution cancelled by batch preflight")
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
		message := "tool execution cancelled by batch preflight"
		if batchErr != nil {
			message = "tool execution cancelled"
		}
		for index, item := range prepared {
			if item == nil {
				prepared[index] = e.cancelledPrepared(requestID, batchID, index, calls[index], queuedAt, message)
			} else if item.terminal == nil {
				result := protocol.ToolResult{CallID: item.call.ID, Content: message, IsError: true, Metadata: map[string]any{"batch_cancelled": true}}
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
		if batchErr == nil && batchCtx.Err() == nil {
			for len(active) < limit {
				// A cancellation observed at any point before this call starts
				// must prevent the call from starting. The cancellation reaches
				// the caller through resolveBatchControl.
				if batchCtx.Err() != nil {
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
				// OnStart may itself observe or trigger a cancellation, so the
				// check is repeated before the call is actually started. The
				// cancellation is reported by resolveBatchControl rather than
				// being folded into the infrastructure failure.
				if batchCtx.Err() != nil {
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
		if batchCtx.Err() != nil {
			// Drain the calls that never started. The cancellation itself is
			// reported by resolveBatchControl, which owns the request-level
			// control state; a batch-local cancel derived from a hook failure is
			// deliberately not reported again, so the original failure stays the
			// leading cause.
			for index, item := range prepared {
				if item.done || item.running {
					continue
				}
				item.state = protocol.CallNotExecuted
				result := protocol.ToolResult{CallID: item.call.ID, Content: "tool execution cancelled", IsError: true, Metadata: map[string]any{"batch_cancelled": true}}
				result, hookErr := e.deliverTerminal(item, result, hooks)
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
			if batchErr == nil && batchCtx.Err() == nil {
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

func (e *Executor) cancelledPrepared(requestID, batchID string, batchIndex int, call protocol.ToolCall, queuedAt time.Time, message string) *preparedCall {
	prepared := &preparedCall{call: call, queuedAt: queuedAt, spec: ConcurrencySpec{Mode: ConcurrencyExclusive}, state: protocol.CallNotExecuted}
	prepared.record = AuditRecord{Timestamp: time.Now().UTC(), RequestID: requestID, BatchID: batchID, BatchIndex: batchIndex, CallID: call.ID, ParentCallID: call.ParentCallID, Tool: call.Name, InputBytes: len(call.Arguments), ConcurrencyMode: string(ConcurrencyExclusive)}
	if e != nil {
		prepared.record.SessionID, prepared.record.ProviderName = e.SessionID, e.ProviderName
		prepared.record.ProviderGeneration, prepared.record.Model = e.ProviderGeneration, e.Model
	}
	result := protocol.ToolResult{CallID: call.ID, Content: message, IsError: true, Metadata: map[string]any{"batch_cancelled": true}}
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
	return result
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
			e.Audit.Record(prepared.record)
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
