package host

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	contextledger "Eylu/internal/context"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/tool"
)

type conversation struct {
	mu                 sync.Mutex
	writes             sync.Mutex
	server             *Server
	state              State
	saved              State
	revision, eventSeq int64
	generation         uint64
	dirty              bool
	failure            *RPCError
	agent              *agent.Conversation
	cancel             context.CancelFunc
	done, ready        chan struct{}
	startErr           error
	started            time.Time
	inflight           map[string]string
	deltas             map[string]int64
}

func newConversation(s *Server, state State) *conversation {
	return &conversation{server: s, state: state, saved: clone(state), revision: state.Revision, eventSeq: state.EventSeq, inflight: map[string]string{}, deltas: map[string]int64{}}
}
func clone[T any](v T) T { b, _ := json.Marshal(v); var out T; _ = json.Unmarshal(b, &out); return out }
func event(kind, run string, data any) Event {
	raw, _ := json.Marshal(data)
	return Event{EventID: uuid.NewString(), Type: kind, RunID: run, Data: raw}
}
func (c *conversation) changed() { c.generation++; c.dirty = true }
func (c *conversation) timeout() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.RunInput != nil {
		return time.Duration(c.state.RunInput.Limits.CallbackTimeoutMS) * time.Millisecond
	}
	return 30 * time.Second
}
func (c *conversation) commit(reason string, events ...Event) error {
	c.writes.Lock()
	defer c.writes.Unlock()
	c.mu.Lock()
	if c.failure != nil {
		err := c.failure
		c.mu.Unlock()
		return err
	}
	state := clone(c.state)
	if c.state.ActiveRun != nil && c.state.ActiveRun.Status == "running" {
		c.state.ActiveRun.Phase = "committing"
	}
	state.Revision = c.revision + 1
	state.EventSeq = c.eventSeq + int64(len(events))
	if events == nil {
		events = []Event{}
	}
	request := Commit{ConversationID: state.ConversationID, CheckpointID: uuid.NewString(), ExpectedRevision: c.revision, BaseEventSeq: c.eventSeq, StateSchema: StateSchema, State: state, Events: events, Reason: reason}
	generation := c.generation
	c.mu.Unlock()
	var response Committed
	var err error
	// A lost commit acknowledgement may be retransmitted once with exactly the
	// same checkpoint ID and content. Paid calls are never retried here.
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(c.server.p.ctx, c.timeout())
		err = c.server.p.call(ctx, "host.checkpoint.commit", request, &response)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			break
		}
	}
	if err == nil {
		if response.Revision != state.Revision || response.EventSeq != state.EventSeq {
			err = fault("CHECKPOINT_FAILED")
		}
		if len(events) == 0 {
			if response.EventSeqRange != nil {
				err = fault("CHECKPOINT_FAILED")
			}
		} else if response.EventSeqRange == nil || response.EventSeqRange.First != request.BaseEventSeq+1 || response.EventSeqRange.Last != state.EventSeq {
			err = fault("CHECKPOINT_FAILED")
		}
	}
	c.mu.Lock()
	if err != nil {
		c.failure = fault("CHECKPOINT_FAILED")
		c.dirty = true
		if c.state.ActiveRun == nil && c.saved.ActiveRun != nil {
			c.state.ActiveRun = clone(c.saved.ActiveRun)
			c.state.LastRun = clone(c.saved.LastRun)
		}
		if c.state.ActiveRun != nil {
			c.state.ActiveRun.Status = "stopping"
			c.state.ActiveRun.BlockingReason = "checkpoint_failed"
		}
		cancel := c.cancel
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return c.failure
	}
	c.revision, c.eventSeq = state.Revision, state.EventSeq
	c.state.Revision, c.state.EventSeq = state.Revision, state.EventSeq
	c.saved = state
	if c.state.ActiveRun != nil && state.ActiveRun != nil && c.state.ActiveRun.RunID == state.ActiveRun.RunID && c.state.ActiveRun.Phase == "committing" {
		c.state.ActiveRun.Phase = state.ActiveRun.Phase
	}
	c.dirty = c.generation != generation
	c.mu.Unlock()
	_ = c.server.p.notify("engine.committed", CommittedNotification{ConversationID: state.ConversationID, CheckpointID: request.CheckpointID, Committed: response})
	return nil
}
func (c *conversation) view() View {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := []string{}
	for _, p := range c.state.PendingCalls {
		ids = append(ids, p.RequestID)
	}
	return clone(View{ConversationID: c.state.ConversationID, Revision: c.revision, EventSeq: c.eventSeq, ActiveRun: c.state.ActiveRun, LastRun: c.state.LastRun, HasUncommittedProgress: c.dirty, BlockingRequestIDs: ids})
}
func (c *conversation) start(v Start) (any, *RPCError) {
	c.server.mu.Lock()
	if c.server.closing {
		c.server.mu.Unlock()
		return nil, fault("SESSION_BUSY")
	}
	c.mu.Lock()
	c.server.mu.Unlock()
	if record, ok := c.state.Runs[v.RunID]; ok {
		if record.Digest != digest(v) {
			c.mu.Unlock()
			return nil, fault("ID_CONFLICT")
		}
		ready := c.ready
		if c.revision >= record.Accepted.Revision {
			c.mu.Unlock()
			return record.Accepted, nil
		}
		c.mu.Unlock()
		if ready != nil {
			select {
			case <-ready:
			case <-c.server.p.ctx.Done():
				return nil, fault("HOST_UNAVAILABLE")
			}
		}
		c.mu.Lock()
		err := c.startErr
		c.mu.Unlock()
		if err != nil {
			return nil, asFault(err, "CHECKPOINT_FAILED")
		}
		return record.Accepted, nil
	}
	if c.failure != nil {
		c.mu.Unlock()
		return nil, fault("CHECKPOINT_FAILED")
	}
	if v.ExpectedRevision != c.revision {
		c.mu.Unlock()
		return nil, fault("STALE_REVISION")
	}
	if c.state.ActiveRun != nil || len(c.state.PendingCalls) > 0 {
		c.mu.Unlock()
		return nil, fault("SESSION_BUSY")
	}
	if c.done != nil {
		select {
		case <-c.done:
		default:
			c.mu.Unlock()
			return nil, fault("SESSION_BUSY")
		}
	}
	engine, err := restoreAgent(c.state)
	if err != nil {
		c.mu.Unlock()
		return nil, fault("STATE_INCOMPATIBLE")
	}
	c.agent = engine
	c.state.RunInput = &v
	c.state.ActiveRun = &Run{RunID: v.RunID, Status: "running", Phase: "preparing", Usage: Usage{Source: "provider"}, KnownEffects: []string{}, UnresolvedRequestIDs: []string{}}
	accepted := Accepted{ConversationID: v.ConversationID, RunID: v.RunID, Revision: c.revision + 1}
	c.state.Runs[v.RunID] = RunRecord{Digest: digest(v), Accepted: accepted}
	c.ready = make(chan struct{})
	c.done = make(chan struct{})
	c.startErr = nil
	c.started = time.Now()
	c.changed()
	ctx, cancel := context.WithTimeout(c.server.p.ctx, time.Duration(v.Limits.MaxElapsedMS)*time.Millisecond)
	c.cancel = cancel
	ready := c.ready
	done := c.done
	c.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
			reason := "cancelled"
			if ctx.Err() == context.DeadlineExceeded {
				reason = "elapsed_limit"
			} else if c.server.p.ctx.Err() != nil {
				reason = "host_unavailable"
			}
			c.mu.Lock()
			if active := c.state.ActiveRun; active != nil && active.RunID == v.RunID && active.Status == "running" {
				active.Status = "stopping"
				if active.Reason == "" {
					active.Reason = reason
				}
				c.changed()
			}
			c.mu.Unlock()
		case <-done:
		}
	}()
	go c.run(ctx, v)
	select {
	case <-ready:
	case <-c.server.p.ctx.Done():
		return nil, fault("HOST_UNAVAILABLE")
	}
	c.mu.Lock()
	err = c.startErr
	c.mu.Unlock()
	if err != nil {
		return nil, asFault(err, "CHECKPOINT_FAILED")
	}
	return accepted, nil
}
func (c *conversation) run(ctx context.Context, v Start) {
	defer func() { c.mu.Lock(); c.cancel(); c.cancel = nil; close(c.done); c.mu.Unlock() }()
	var readyOnce sync.Once
	ack := func(err error) { readyOnce.Do(func() { c.mu.Lock(); c.startErr = err; close(c.ready); c.mu.Unlock() }) }
	host := &agent.HostRuntime{Instructions: c.state.Instructions, Context: []string{}, ContextCommitted: c.compacted}
	for _, item := range c.state.Context {
		host.Context = append(host.Context, item.Source+"\n"+item.Text)
	}
	modelRuntime := agent.Runtime{Host: host, Driver: &modelPort{c: c}, PermissionMode: "host", Provider: provider.Snapshot{Name: v.ModelBinding.BindingID, Generation: uint64(v.ModelBinding.BindingRevision), Config: config.ProviderConfig{Model: v.ModelBinding.BindingID, Adapter: "host", ContextWindow: v.ModelBinding.ContextWindowTokens}}, Timeout: time.Duration(v.Limits.CallbackTimeoutMS) * time.Millisecond, OutputReserveTokens: min(v.ModelBinding.MaxOutputTokens, v.Limits.MaxOutputTokensTotal), ContextEvent: func(contextledger.Event) {}}
	registry := tool.NewRegistry()
	for _, definition := range v.ToolCatalog.Tools {
		_ = registry.Register(&toolPort{c: c, definition: definition})
	}
	executor := &tool.Executor{Registry: registry, Policy: hostPolicy{}, OpaqueInputs: true, MaxParallelTools: 1, MaxOutputBytes: MaxMessageBytes, SessionID: v.ConversationID}
	var report agent.RunReport
	lifecycle := agent.RequestLifecycle{
		UserCommitted: func(turn protocol.Turn) error {
			message := storedTurn(turn)
			c.mu.Lock()
			c.state.Messages = append(c.state.Messages, message)
			c.changed()
			c.mu.Unlock()
			err := c.commit("run.started", event("message.committed", v.RunID, message), event("run.started", v.RunID, v))
			ack(err)
			return err
		},
		TurnCommitted: c.turnCommitted, ToolPrepared: c.toolPrepared,
		Finished: func(r agent.RunReport) error { return c.finished(ctx, r) },
	}
	options, err := lifecycle.Prepare(agent.LoopOptions{MaxTurns: v.Limits.MaxModelCalls, RequestID: v.RunID, Report: &report})
	if err == nil {
		_, err = c.agent.Run(ctx, v.Input.Text, modelRuntime, executor, options, false, nil)
	}
	ack(err)
	_ = lifecycle.Settle(&report, err)
}
func (c *conversation) turnCommitted(turn protocol.Turn) error {
	message := storedTurn(turn)
	c.mu.Lock()
	run := c.state.ActiveRun.RunID
	if turn.Role == protocol.RoleTool {
		parts := []Part{}
		for _, p := range message.Message.Parts {
			if !hasToolResult(c.state.Messages, p.ToolCallID) {
				parts = append(parts, p)
			}
		}
		message.Message.Parts = parts
	}
	for _, existing := range c.state.Messages {
		if existing.MessageID == message.MessageID {
			c.mu.Unlock()
			return nil
		}
	}
	if len(message.Message.Parts) == 0 {
		c.mu.Unlock()
		return nil
	}
	c.state.Messages = append(c.state.Messages, message)
	c.changed()
	c.mu.Unlock()
	return c.commit("message.committed", event("message.committed", run, message))
}
func (c *conversation) compacted() error {
	state := c.agent.ExportState()
	omitted := map[string]bool{}
	for _, id := range state.OmittedTurnIDs {
		omitted[id] = true
	}
	visible := map[string]bool{}
	for _, turn := range state.Turns {
		if !omitted[turn.ID] {
			visible[turn.ID] = true
			for _, p := range turn.Parts {
				if p.ToolResult != nil {
					visible[p.ToolResult.CallID] = true
				}
			}
		}
	}
	c.mu.Lock()
	messages := []StoredMessage{}
	for _, m := range c.state.Messages {
		keep := visible[m.MessageID]
		for _, p := range m.Message.Parts {
			if p.Type == "tool_result" && visible[p.ToolCallID] {
				keep = true
			}
		}
		if keep {
			messages = append(messages, m)
		}
	}
	c.state.Messages = messages
	c.state.Summary = state.Summary
	c.changed()
	run := c.state.ActiveRun.RunID
	c.mu.Unlock()
	return c.commit("context.compacted", event("context.compacted", run, map[string]any{"summary": state.Summary}))
}
func (c *conversation) finished(ctx context.Context, report agent.RunReport) error {
	c.mu.Lock()
	if c.failure != nil {
		c.mu.Unlock()
		return fault("CHECKPOINT_FAILED")
	}
	run := c.state.ActiveRun
	if run == nil {
		c.mu.Unlock()
		return nil
	}
	if run.Reason == "" {
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			run.Reason = "elapsed_limit"
			run.Status = "interrupted"
		case ctx.Err() != nil:
			run.Reason = "cancelled"
			run.Status = "interrupted"
		case report.StopReason == "completed":
			run.Reason = "completed"
			run.Status = "completed"
		case report.StopReason == "length":
			run.Reason = "output_limit"
			run.Status = "failed"
		case report.StopReason == "iteration_limit":
			run.Reason = "model_call_limit"
			run.Status = "interrupted"
		default:
			run.Reason = "model_request_failed"
			run.Status = "failed"
		}
	} else if run.Status == "stopping" {
		run.Status = "interrupted"
	} else if run.Status == "running" {
		run.Status = "failed"
	}
	run.Phase = ""
	run.UnresolvedRequestIDs = []string{}
	for _, p := range c.state.PendingCalls {
		run.UnresolvedRequestIDs = append(run.UnresolvedRequestIDs, p.RequestID)
	}
	if len(run.UnresolvedRequestIDs) > 0 {
		run.BlockingReason = "reconciliation_required"
	}
	c.state.LastRun = run
	c.state.ActiveRun = nil
	events := c.closeUnsentLocked(run.RunID)
	record := c.state.Runs[run.RunID]
	record.Outcome = clone(run)
	c.state.Runs[run.RunID] = record
	c.changed()
	snapshot := clone(*run)
	c.mu.Unlock()
	events = append(events, event("run.finished", snapshot.RunID, snapshot))
	return c.commit("run.finished", events...)
}

// closeUnsentLocked records only calls known not to have reached the host. An
// in-flight or uncertain call keeps its independent reconciliation record.
func (c *conversation) closeUnsentLocked(runID string) []Event {
	prepared := map[string]bool{}
	for _, p := range c.state.PendingCalls {
		if p.Kind == "tool" {
			var r ToolRequest
			_ = json.Unmarshal(p.Request, &r)
			prepared[r.ToolCallID] = true
		}
	}
	events := []Event{}
	for _, m := range append([]StoredMessage{}, c.state.Messages...) {
		for _, p := range m.Message.Parts {
			if p.Type != "tool_call" || prepared[p.ToolCallID] || hasToolResult(c.state.Messages, p.ToolCallID) {
				continue
			}
			result := notDispatched(p.ToolCallID, "Run ended before this tool was dispatched")
			closed := StoredMessage{MessageID: "closed:" + digest(p.ToolCallID), Message: Message{Role: "tool", Parts: []Part{{Type: "tool_result", ToolCallID: p.ToolCallID, Result: result}}}}
			c.state.Messages = append(c.state.Messages, closed)
			events = append(events, event("message.committed", runID, closed))
		}
	}
	return events
}
func (c *conversation) stop(reason string) {
	c.mu.Lock()
	if c.state.ActiveRun == nil {
		c.mu.Unlock()
		return
	}
	if c.state.ActiveRun.Status != "stopping" {
		c.state.ActiveRun.Status = "stopping"
		c.state.ActiveRun.Reason = reason
		c.changed()
	}
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
func (c *conversation) interrupt(v Interrupt) (any, *RPCError) {
	c.mu.Lock()
	if c.state.ActiveRun == nil || c.state.ActiveRun.RunID != v.RunID {
		record, known := c.state.Runs[v.RunID]
		last := clone(record.Outcome)
		c.mu.Unlock()
		if !known {
			return nil, fault("NOT_FOUND")
		}
		return map[string]any{"accepted": false, "run_id": v.RunID, "last_run": last}, nil
	}
	if c.state.ActiveRun.Status != "stopping" {
		c.state.ActiveRun.Status = "stopping"
		c.state.ActiveRun.Reason = v.Reason
		c.changed()
	}
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return map[string]any{"accepted": true, "run_id": v.RunID, "status": "stopping"}, nil
}
func (c *conversation) failRun(reason string) {
	c.mu.Lock()
	if c.state.ActiveRun != nil && c.state.ActiveRun.Reason == "" {
		c.state.ActiveRun.Reason = reason
		c.changed()
	}
	c.mu.Unlock()
}
func (c *conversation) delta(d Delta) {
	c.mu.Lock()
	valid := c.state.ActiveRun != nil && c.state.ActiveRun.Status == "running" && c.state.ActiveRun.RunID == d.RunID && c.inflight[d.RequestID] == "model" && d.DeltaSeq == c.deltas[d.RequestID]+1
	if valid {
		c.deltas[d.RequestID] = d.DeltaSeq
	}
	c.mu.Unlock()
	if valid {
		_ = c.server.p.notify("engine.progress", d)
	}
}
func (c *conversation) progress(p CallProgress) {
	c.mu.Lock()
	valid := c.state.ActiveRun != nil && c.state.ActiveRun.Status == "running" && c.state.ActiveRun.RunID == p.RunID && c.inflight[p.RequestID] != ""
	c.mu.Unlock()
	if valid {
		_ = c.server.p.notify("engine.progress", p)
	}
}
func hasToolResult(messages []StoredMessage, id string) bool {
	for _, m := range messages {
		for _, p := range m.Message.Parts {
			if p.Type == "tool_result" && p.ToolCallID == id {
				return true
			}
		}
	}
	return false
}
func (c *conversation) removePending(id string) {
	c.state.PendingCalls = slices.DeleteFunc(c.state.PendingCalls, func(p PendingCall) bool { return p.RequestID == id })
}
