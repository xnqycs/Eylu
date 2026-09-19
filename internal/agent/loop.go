package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
	"Eylu/internal/webtool"
)

type LoopOptions struct {
	MaxTurns       int
	MaxTotalTokens int
	RequestID      string
	BeforeModel    func() string
	// Usage, when non-nil, receives the accumulated usage of this run. The
	// response returned by Run keeps its own usage, which describes only the
	// final model call.
	Usage *RunUsage
	// Report, when non-nil, receives the durable summary of this run: why it
	// stopped, what executed, what stayed unknown and what it cost.
	Report *RunReport
	// OnTurnCommitted, when non-nil, is called with every turn immediately after
	// it is committed to the transcript, in transcript order: the model turn as
	// soon as the response is validated, and the tool turn as soon as its results
	// are recorded.
	//
	// It is how a host makes the conversation durable turn by turn instead of
	// once at the end of the request, so a crash in the middle loses at most the
	// turn that was still in flight. A failure is a persistence fault: the results
	// already in memory are kept and no new side effect starts, because a request
	// that cannot record what it did must not do more.
	OnTurnCommitted func(protocol.Turn) error
	// OnToolPrepared, when non-nil, is called for every call the executor accepts
	// for execution, from the same place the conversation marks the call prepared.
	// It is the point after which the call may have had an effect and before which
	// it provably has not, so a host that records it can tell "never started" from
	// "outcome unknown" without guessing.
	OnToolPrepared func(protocol.ToolCall) error
}

var ErrRequestInterrupted = errors.New("request interrupted by user")

// TurnPersistError reports that a committed turn could not be made durable.
//
// The turn is already part of the in-memory transcript and its results are kept:
// the failure says the record is behind, not that the work was undone. Because a
// request that cannot record what it did must not do more, it also ends the
// request instead of letting the next batch start.
type TurnPersistError struct {
	TurnID string
	Cause  error
}

// toolHooksWithPrepared wraps the batch hooks so the pending set and the host both
// learn that a call was accepted for execution.
//
// The wrapper is additive: the event projection still decides what the host is
// told, and a hook failure is reported exactly as before. Marking the call prepared
// happens first, so a call the executor accepted is recorded as such even when the
// host event cannot be delivered - the failure stops the batch, and a call that was
// accepted but never reported must not look like one that never started.
func toolHooksWithPrepared(hooks tool.BatchHooks, conversation *Conversation, onPrepared func(protocol.ToolCall) error) tool.BatchHooks {
	if conversation == nil {
		return hooks
	}
	inner := hooks.OnStart
	hooks.OnStart = func(call protocol.ToolCall) error {
		conversation.markCallPrepared(call.ID)
		if onPrepared != nil {
			if err := onPrepared(call); err != nil {
				return err
			}
		}
		if inner == nil {
			return nil
		}
		return inner(call)
	}
	return hooks
}

// recordBatchResultsLocked puts the terminal state of every executed call into the
// pending set, so a call that ran is closed by the set rather than by a later scan
// of the transcript. The caller holds the state lock: it is called from inside the
// commit of the tool turn, so the two records are written together.
func (c *Conversation) recordBatchResultsLocked(results []protocol.ToolResult) {
	for _, result := range results {
		if entry := c.pendingEntryLocked(result.CallID); entry != nil && entry.State == "" {
			entry.State = result.State
		}
	}
}
func (e *TurnPersistError) Error() string {
	return fmt.Sprintf("the committed turn %s could not be persisted: %v", e.TurnID, e.Cause)
}

func (e *TurnPersistError) Unwrap() error { return e.Cause }

// commitTurn hands one committed turn to the host. A host without a persistence
// hook is a host that persists the whole conversation later, which is what every
// caller did before the hook existed.
func commitTurn(hook func(protocol.Turn) error, turn protocol.Turn) error {
	if hook == nil {
		return nil
	}
	if err := hook(turn); err != nil {
		return &TurnPersistError{TurnID: turn.ID, Cause: err}
	}
	return nil
}

// StopNote describes a stop reason that must not be presented as a normal
// completion. It returns an empty string for a genuine completion.
func StopNote(stop protocol.StopKind) string {
	switch stop {
	case protocol.StopLength:
		return "the provider stopped the response before it finished, so the answer may be incomplete"
	case protocol.StopCancelled:
		return "the provider reported that the response was cancelled"
	default:
		return ""
	}
}

// RunStopNote describes why one request ended, in the one wording every output
// uses.
//
// It answers for the request as a whole rather than for the final response: a
// request can also end at its token budget, on a persistence fault, because the
// host narrowed a safety setting, or because the host event consumer failed, and
// none of those is a protocol stop kind. The text output and the TUI history both
// read this one function, so the same request cannot be described two ways - which
// is exactly what an answer shown without a note gets wrong.
func RunStopNote(stop protocol.StopKind, report RunReport) string {
	// For the reasons only a request can have, the report's own error is already the
	// sentence a human should read, so it is used rather than reworded.
	if detail := strings.TrimSpace(report.Error); detail != "" {
		switch report.StopReason {
		case stopPolicyTightened, stopPersistenceFailed, eventSinkFailedStop:
			return detail
		}
	}
	switch report.StopReason {
	case stopPolicyTightened:
		return "the request stopped because the safety settings were tightened"
	case stopPersistenceFailed:
		return "the request stopped because a committed turn could not be persisted"
	case eventSinkFailedStop:
		return "the request stopped because the host event consumer failed"
	case stopTokenBudget:
		return "the request stopped at its token budget, so the work it had done is kept and nothing new was started"
	case stopIterationLimit:
		return "the request stopped at its turn limit, so the work it had done is kept"
	case stopInterrupted:
		return "the request was interrupted"
	case string(protocol.StopError):
		return "the model reported a failed response"
	}
	if note := StopNote(stop); note != "" {
		return note
	}
	switch report.StopReason {
	case stopCancelled, stopAborted:
		return "the request did not finish"
	}
	return ""
}

// Run performs one request.
//
// The conversation has a single writer: Run claims it for the whole request and a
// concurrent Run is refused with ErrConversationBusy. The state mutex is only
// held for short reads and commits, so the model call, the approval callback, the
// tool batch, the host event callbacks and every host-supplied function run with
// the state unlocked. A caller may therefore read ContextReport or ExportState
// from inside a callback, and during a blocked model call.
func (c *Conversation) Run(ctx context.Context, prompt string, runtime Runtime, executor *tool.Executor, options LoopOptions, stream bool, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	if err := c.gate.begin(); err != nil {
		return protocol.ModelResponse{}, err
	}
	defer c.gate.release()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.gate.attach(cancel)
	ctx = runCtx

	c.mu.Lock()
	if err := c.prepareRuntime(prompt, runtime); err != nil {
		c.mu.Unlock()
		return protocol.ModelResponse{}, err
	}
	if executor == nil {
		c.mu.Unlock()
		return protocol.ModelResponse{}, fmt.Errorf("tool executor is nil")
	}
	baseTools := registryToolsExcluding(executor.Registry, runtime.MCPToolServers)
	c.appendUser(prompt)
	c.mu.Unlock()

	webBudget := webtool.NewUsageBudget()
	hostedAuthorized := false
	var err error
	runtime, err = c.refreshMCPRuntime(runtime, executor, baseTools)
	if err != nil {
		return protocol.ModelResponse{}, err
	}
	maxTurns := options.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 20
	}
	plan, err := c.resolveWebRuntime(runtime, executor, webBudget)
	if err != nil {
		return protocol.ModelResponse{}, err
	}
	if err := authorizeHostedWeb(ctx, executor, runtime, plan, &hostedAuthorized); err != nil {
		return protocol.ModelResponse{}, err
	}
	c.applyToolDefinitions(runtime, plan.Definitions)
	// definitions is assigned per round from the resolved plan, so it is declared
	// here and never carries a value across iterations.
	var definitions []protocol.ToolDefinition
	seenCalls := make(map[string]struct{})
	budget := newBudgetTracker(options.MaxTotalTokens)
	if options.Usage != nil {
		*options.Usage = RunUsage{}
	}
	requestID := options.RequestID
	if requestID == "" {
		requestID = uuid.NewString()
	}
	if options.Report != nil {
		*options.Report = RunReport{SchemaVersion: RunReportSchemaVersion, RequestID: requestID}
	}
	// finalizer owns every exit path of this request: it closes pending calls,
	// publishes the run usage and fills the report.
	finalizer := &runFinalizer{
		conversation: c, runtime: runtime, budget: budget,
		usage: options.Usage, report: options.Report,
	}
	if executor != nil {
		finalizer.auditFailures = executor.AuditFailures
		finalizer.auditDetail = executor.AuditFailureDetail
	}
	// Host events go through a coalescing queue: streamed text is merged within a
	// bound so a burst never stalls the model, while every terminal, control or
	// content event is delivered synchronously and its failure stops the request.
	if emit != nil {
		sink := emit
		queue := startEventQueue(func(event protocol.ModelEvent) error { return sink(event) })
		finalizer.events = queue
		emit = queue.push
	}
	// finish ends the request with the reason it stopped.
	finish := func(response protocol.ModelResponse, last protocol.ModelResponse, err error, stop string) (protocol.ModelResponse, error) {
		return finalizer.finish(response, last, err, stop)
	}
	var last protocol.ModelResponse
	for iteration := 0; iteration < maxTurns; iteration++ {
		finalizer.iterations = iteration + 1
		if err := ctx.Err(); err != nil {
			return finish(protocol.ModelResponse{}, last, err, stopCancelled)
		}
		// A provider change that arrived during the previous round is applied here,
		// at the round boundary, instead of in the middle of a request.
		if snapshot, ok := c.takePendingSnapshot(); ok {
			c.mu.Lock()
			c.applyProviderSnapshotLocked(snapshot)
			c.mu.Unlock()
		}
		// Host callbacks run outside the state lock.
		if options.BeforeModel != nil {
			if message := strings.TrimSpace(options.BeforeModel()); message != "" {
				c.mu.Lock()
				c.appendUser(message)
				c.rebuildLedger(runtime)
				c.mu.Unlock()
			}
		}
		runtime, err = c.refreshMCPRuntime(runtime, executor, baseTools)
		if err != nil {
			return finish(protocol.ModelResponse{}, last, err, stopAborted)
		}
		plan, err = c.resolveWebRuntime(runtime, executor, webBudget)
		if err != nil {
			return finish(protocol.ModelResponse{}, last, err, stopAborted)
		}
		if err := authorizeHostedWeb(ctx, executor, runtime, plan, &hostedAuthorized); err != nil {
			return finish(protocol.ModelResponse{}, last, err, stopAborted)
		}
		definitions = plan.Definitions
		c.applyToolDefinitions(runtime, definitions)
		parallelToolCalls := executor.ParallelLimit() > 1 && driver.CapabilitiesFor(runtime.Driver, capabilityTarget(runtime)).ParallelTools
		response, effectiveRuntime, err := c.generate(ctx, runtime, definitions, parallelToolCalls, stream, emit, budget)
		if err != nil {
			// The last usable response is preserved, so a caller that stops on a
			// budget still sees the work that did happen. A budget stop is reported
			// as such whether the limit was hit by a call or by the admission check
			// that refused to start one.
			stop := stopAborted
			if errors.Is(err, ErrTokenBudget) {
				stop = stopTokenBudget
			}
			return finish(protocol.ModelResponse{}, last, err, stop)
		}
		// The call happened and the provider reported what it cost, so the usage is
		// recorded here, before the response is validated or persisted. Whether the
		// turn can be written says nothing about whether the model was already paid
		// for, and a request that fails afterwards must still report what it spent.
		budget.add(callMain, response.Usage)
		// Validate before committing: a malformed response must not become part
		// of the history the next request is built from.
		response, err = normalizeModelResponse(response, seenCalls)
		if err != nil {
			c.mu.Lock()
			c.driverState = nil
			c.rebuildLedger(runtime)
			c.mu.Unlock()
			// A refused response is an outcome like any other, so it leaves through
			// the one exit path: the report states why the request ended, the usage
			// is published, the buffered host events are delivered and the pending
			// set is released. Returning directly here left the report empty and the
			// set behind for the next request.
			return finish(protocol.ModelResponse{}, last, err, stopAborted)
		}
		c.mu.Lock()
		c.commitResponse(response, effectiveRuntime)
		// The calls of this turn enter the pending set at the moment the turn is
		// committed, so the set describes the transcript rather than a later view.
		c.trackCommittedCalls(requestID, iteration+1, response.Turn)
		c.mu.Unlock()
		// An interoperability relaxation the provider needed is durable evidence,
		// so it is recorded on the report instead of only existing in the driver
		// that applied it.
		recordInterop(options.Report, response.Interop)
		last = response
		for _, call := range toolCalls(response.Turn) {
			seenCalls[call.ID] = struct{}{}
		}
		// Every committed call must end with a terminal state. The finalizer owns
		// that exit path and leaves a call that already has a result untouched.
		calls := toolCalls(response.Turn)
		finalizer.calls = calls
		closePending := finalizer.closePending
		// The turn is committed, so the host gets the chance to make it durable
		// before any of its calls can run. A failure here is a persistence fault:
		// the calls are closed without being executed and the request ends,
		// because a request that cannot record what it did must not do more.
		if err := commitTurn(options.OnTurnCommitted, response.Turn); err != nil {
			closePending("tool call was not executed: the committed turn could not be persisted")
			return finish(protocol.ModelResponse{}, last, err, stopPersistenceFailed)
		}
		if err := recordHostedWebActivities(executor, runtime, requestID, response.Turn, plan, webBudget); err != nil {
			closePending("tool call was not executed: hosted web activity was rejected")
			return finish(protocol.ModelResponse{}, last, err, stopAborted)
		}
		if budget.exceeded() {
			// The request spent more than it was allowed to. The response that
			// caused it is kept and its calls are closed without executing, so the
			// history stays usable and no side effect runs on an exhausted budget.
			closePending("tool call was not executed: the request token budget was exhausted before execution")
			return finish(protocol.ModelResponse{}, last, budgetError(options.MaxTotalTokens, false), stopTokenBudget)
		}
		// The stop reason decides what happens next. Only a genuine completion or a
		// tool-use request is a normal outcome; everything else is reported as
		// such instead of being presented as a completion.
		switch response.Stop {
		case protocol.StopCompleted:
			return finish(response, last, nil, string(response.Stop))
		case protocol.StopLength, protocol.StopCancelled:
			closePending("tool call was not executed: " + StopNote(response.Stop))
			return finish(response, last, nil, string(response.Stop))
		case protocol.StopError:
			closePending("tool call was not executed: the provider reported a failed response")
			return finish(protocol.ModelResponse{}, last, &protocol.Error{Code: protocol.ErrProtocol, Message: "model reported a failed response"}, string(response.Stop))
		}
		if len(calls) == 0 {
			closePending("tool call was not executed: the model requested tool use without tool calls")
			return finish(protocol.ModelResponse{}, last, &protocol.Error{Code: protocol.ErrProtocol, Message: "model stopped for tool use without tool calls"}, stopAborted)
		}
		toolTurn := protocol.Turn{ID: uuid.NewString(), Role: protocol.RoleTool, CreatedAt: time.Now().UTC()}
		runtime, err = c.refreshMCPRuntime(runtime, executor, baseTools)
		if err != nil {
			closePending("tool call was not executed: the tool registry could not be refreshed")
			return finish(protocol.ModelResponse{}, last, err, stopAborted)
		}
		plan, err = c.resolveWebRuntime(runtime, executor, webBudget)
		if err != nil {
			closePending("tool call was not executed: web tools could not be resolved")
			return finish(protocol.ModelResponse{}, last, err, stopAborted)
		}
		finalizer.runtime = runtime
		definitions = plan.Definitions
		c.applyToolDefinitions(runtime, definitions)
		expansion := expandToolCalls(calls, plan, newExecutionAllocator(callIDs(calls)))
		hooks := toolHooksWithPrepared(projectToolEvents(emit, expansion), c, options.OnToolPrepared)
		limit := executor.ParallelLimit()
		if len(expansion.web) > limit {
			limit = min(len(expansion.web), webtool.MaxBatchQueries)
		}
		executedResults, batchOutcome := executor.ExecuteBatchOutcome(ctx, requestID, expansion.calls, hooks, limit)
		finalizer.countBatch(batchOutcome.States)
		results, webParts := collapseToolResults(calls, expansion, executedResults, batchOutcome)
		// The batch results are committed under the state lock, after every tool
		// and every callback has finished.
		c.mu.Lock()
		for index := range results {
			result := results[index]
			c.captureSkillResult(result)
			c.captureTodoListResult(result)
			toolTurn.Parts = append(toolTurn.Parts, protocol.Part{Kind: protocol.PartToolResult, ToolResult: &result})
		}
		toolTurn.Parts = append(toolTurn.Parts, webParts...)
		c.turns = append(c.turns, toolTurn)
		// The set learns the terminal state of every executed call from the same
		// results the transcript is built from, so the two cannot disagree.
		c.recordBatchResultsLocked(results)
		c.projectMapDirty = true
		if batchOutcome.Control != protocol.ControlContinue {
			// A remote driver state built from a response whose calls did not all
			// complete cannot be reused; the next request rebuilds it from the
			// local transcript.
			c.driverState = nil
		}
		c.rebuildLedger(runtime)
		c.mu.Unlock()
		// The tool turn is committed too, so the results of this batch are made
		// durable before the next round can start.
		if err := commitTurn(options.OnTurnCommitted, toolTurn); err != nil {
			return finish(protocol.ModelResponse{}, last, err, stopPersistenceFailed)
		}
		// The request-level control state is owned by the executor. It is never
		// derived from tool content or from metadata rebuilt by Web aggregation,
		// so an external tool cannot interrupt or abort the host request.
		switch batchOutcome.Control {
		case protocol.ControlInterruptRequest:
			return finish(protocol.ModelResponse{}, last, ErrRequestInterrupted, stopInterrupted)
		case protocol.ControlCancelRequest:
			return finish(protocol.ModelResponse{}, last, batchOutcome.Cause, stopCancelled)
		case protocol.ControlAbortRequest:
			return finish(protocol.ModelResponse{}, last, batchOutcome.Cause, stopAborted)
		}
	}
	return finish(protocol.ModelResponse{}, last,
		&protocol.Error{Code: protocol.ErrProtocol, Message: fmt.Sprintf("agent iteration limit exceeded (%d)", maxTurns)},
		stopIterationLimit)
}

func (c *Conversation) captureTodoListResult(result protocol.ToolResult) {
	if result.IsError || result.TodoList == nil {
		return
	}
	c.todoList = cloneTodoList(*result.TodoList)
}

func toolCalls(turn protocol.Turn) []protocol.ToolCall {
	result := make([]protocol.ToolCall, 0)
	for _, part := range turn.Parts {
		if part.Kind == protocol.PartToolCall && part.ToolCall != nil {
			result = append(result, *part.ToolCall)
		}
	}
	return result
}

// callIDs lists the model call IDs of one response. They are reserved for the
// execution allocator, so a host execution can never take an identity the model
// already used.
func callIDs(calls []protocol.ToolCall) []string {
	ids := make([]string, 0, len(calls))
	for _, call := range calls {
		ids = append(ids, call.ID)
	}
	return ids
}
