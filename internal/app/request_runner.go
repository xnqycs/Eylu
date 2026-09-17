package app

import (
	"context"
	"time"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	"Eylu/internal/metrics"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/skill"
	"Eylu/internal/tool"
)

// toolRun is the durable side of one request: the executor its tools run on, the
// lifecycle records written around them, the request deadline, and the way the
// outcome is persisted.
//
// There is deliberately one implementation of it for every entry point. A front
// end may differ in how it asks for approval, renders events and prints its answer;
// it must not differ in whether a side-effecting call records its intent before it
// runs, whether the conversation is made durable turn by turn, or whether the
// reason a request stopped reaches the log. Those were patched into one entry point
// at a time until this type existed, which is exactly the class of defect it
// removes: a front end cannot forget a guarantee it never had to install.
type toolRun struct {
	runtime      *runtime
	session      *sessionRuntime
	conversation *agent.Conversation
	manager      *provider.Manager
	options      chatOptions
}

// newToolRun binds one request to the session it persists into.
//
// A nil session is a request without durable state, which is what a run with no
// session file uses; every hook below then simply does nothing rather than being
// installed conditionally at each call site.
func (r *runtime) newToolRun(session *sessionRuntime, conversation *agent.Conversation, manager *provider.Manager, options chatOptions) *toolRun {
	return &toolRun{runtime: r, session: session, conversation: conversation, manager: manager, options: options}
}

// requestContext bounds one request. A model or a tool that never returns must not
// hold the conversation forever, and every entry point gets the same bound.
func (t *toolRun) requestContext(ctx context.Context, cfg config.Config, modelRuntime agent.Runtime) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, time.Duration(cfg.MaxTurns)*modelRuntime.Timeout)
}

// executor builds the tool executor of this request and binds it to the session.
//
// Building and binding are one step on purpose: an executor that runs a side effect
// without a checkpoint is a change to the workspace that nothing recorded, and it
// is not something a call site should be able to ask for.
func (t *toolRun) executor(cfg config.Config, modelRuntime agent.Runtime, skills *skill.Registry, skillSession *skill.Session, confirm tool.ConfirmFunc, ask tool.AskFunc, audit tool.AuditSink) (*tool.Executor, error) {
	executor, err := t.runtime.toolExecutorWith(cfg, t.options, skills, skillSession, confirm, ask, audit)
	if err != nil {
		return nil, err
	}
	executor.SessionID = t.conversation.SessionID()
	executor.ProviderName = modelRuntime.Provider.Name
	executor.ProviderGeneration = modelRuntime.Provider.Generation
	executor.Model = modelRuntime.Provider.Config.Model
	if t.session != nil {
		executor.Checkpoint = t.session
	}
	return executor, nil
}

// prepare records that the request started and returns the loop options that make
// it durable while it runs.
//
// The turn hook and the prepared-call hook are installed together with the
// request-start record, so the log and the pending set agree about which request
// owns a call from the first moment the call can have an effect.
func (t *toolRun) prepare(cfg config.Config, requestID string, report *agent.RunReport, usage *agent.RunUsage) (agent.LoopOptions, error) {
	options := agent.LoopOptions{
		MaxTurns: cfg.MaxTurns, MaxTotalTokens: cfg.MaxTotalTokens, RequestID: requestID,
		Report: report, Usage: usage,
	}
	if t.session == nil {
		return options, nil
	}
	session, conversation := t.session, t.conversation
	// Every committed turn is written while the request is still running, so a crash
	// in the middle cannot lose a turn whose side effect already happened.
	options.OnTurnCommitted = session.RecordTurn
	// The prepared-event source is the pending set of this conversation, so the log
	// and the view cannot disagree about which call was prepared.
	options.OnToolPrepared = func(call protocol.ToolCall) error { return session.RecordToolPrepared(conversation, call) }
	if err := session.RecordRequestStarted(requestID); err != nil {
		return agent.LoopOptions{}, err
	}
	return options, nil
}

// requestMetric finishes one request's observation and adds its totals.
//
// The metric describes one request rather than its last model call, and the same
// object is produced on every entry point; only where it is written differs.
func requestMetric(observation *metrics.Observation, response protocol.ModelResponse, usage agent.RunUsage, runErr error) metrics.RequestMetric {
	metric := observation.Finish(response.Usage, runErr)
	metric.RequestUsage, metric.RequestModelCalls = requestUsageTotals(usage)
	return metric
}

// settle persists the outcome of one request and reports the error that must reach
// the caller.
//
// The run summary is written first, so the reason a request stopped survives even
// when the snapshot write fails; the sync error leads because it means the session
// state itself is behind, and the summary error is returned when the sync
// succeeded. A nil report means the request never reached its execution phase, so
// only the snapshot is written.
func (t *toolRun) settle(report *agent.RunReport, runErr error) error {
	if t.session == nil {
		return nil
	}
	var reportErr error
	if report != nil {
		reportErr = t.session.RecordRunReport(*report)
	}
	syncErr := t.session.Sync(t.conversation, t.manager, t.options, runErr)
	if syncErr == nil {
		syncErr = reportErr
	}
	return syncErr
}
