package host

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"Eylu/internal/agent"
	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type modelPort struct{ c *conversation }

func (*modelPort) Name() string { return "host" }
func (*modelPort) Capabilities() driver.Capabilities {
	return driver.Capabilities{TextStreaming: true, ToolCalling: true}
}
func (p *modelPort) Generate(ctx context.Context, in driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	c := p.c
	if ctx.Err() != nil {
		return protocol.ModelResponse{}, ctx.Err()
	}
	messages := make([]Message, 0, len(in.Model.Turns))
	for _, t := range in.Model.Turns {
		messages = append(messages, storedTurn(t).Message)
	}
	c.mu.Lock()
	v := *c.state.RunInput
	run := c.state.ActiveRun
	if run.Status != "running" || c.failure != nil {
		c.mu.Unlock()
		return protocol.ModelResponse{}, fault("SESSION_BUSY")
	}
	if run.ModelCalls >= v.Limits.MaxModelCalls || run.Usage.OutputTokens >= v.Limits.MaxOutputTokensTotal || run.Usage.Source == "unknown" {
		c.mu.Unlock()
		c.failRun("budget_limit")
		return protocol.ModelResponse{}, fault("LIMIT_EXCEEDED")
	}
	remaining := RemainingLimits{ModelCalls: v.Limits.MaxModelCalls - run.ModelCalls, OutputTokens: v.Limits.MaxOutputTokensTotal - run.Usage.OutputTokens, ElapsedMS: max(0, v.Limits.MaxElapsedMS-time.Since(c.started).Milliseconds())}
	remaining.MaxOutputTokens = min(v.ModelBinding.MaxOutputTokens, remaining.OutputTokens)
	if remaining.ElapsedMS == 0 {
		c.mu.Unlock()
		c.stop("elapsed_limit")
		return protocol.ModelResponse{}, fault("LIMIT_EXCEEDED")
	}
	request := ModelRequest{ConversationID: v.ConversationID, RunID: v.RunID, RequestID: uuid.NewString(), ModelBinding: v.ModelBinding, Purpose: in.Purpose, Messages: messages, Tools: []ToolDefinition{}, RemainingLimits: remaining}
	if request.Purpose == "" {
		request.Purpose = "conversation"
	}
	if request.Purpose == "conversation" {
		request.Tools = v.ToolCatalog.Tools
	}
	raw, _ := json.Marshal(request)
	run.ModelCalls++
	run.Phase = "waiting_model"
	c.state.PendingCalls = append(c.state.PendingCalls, PendingCall{RequestID: request.RequestID, RunID: v.RunID, Kind: "model", Digest: digest(request), Request: raw})
	c.changed()
	c.mu.Unlock()
	if err := c.commit("call.prepared", event("call.prepared", v.RunID, request)); err != nil {
		return protocol.ModelResponse{}, err
	}
	var result ModelResult
	if err := c.callback(ctx, "host.model.generate", request.RequestID, "model", request, &result); err != nil {
		c.mu.Lock()
		c.state.ActiveRun.Usage.Source = "unknown"
		c.changed()
		c.mu.Unlock()
		if rpcErr := asFault(err, "MODEL_REQUEST_FAILED"); rpcErr.Data.Code == "UNSUPPORTED_CAPABILITY" {
			c.failRun("unsupported_capability")
		} else {
			c.failRun("model_request_failed")
		}
		return protocol.ModelResponse{}, err
	}
	result = normalizeStoppedModel(result)
	if err := validateModelResult(result, request.RequestID); err != nil {
		c.mu.Lock()
		c.state.ActiveRun.Usage.Source = "unknown"
		c.changed()
		c.mu.Unlock()
		c.failRun("invalid_model_result")
		return protocol.ModelResponse{}, err
	}
	c.mu.Lock()
	for _, part := range result.Message.Parts {
		if part.Type == "tool_call" && slices.Contains(c.state.UsedToolCallIDs, part.ToolCallID) {
			c.state.ActiveRun.Usage.Source = "unknown"
			c.changed()
			c.mu.Unlock()
			c.failRun("duplicate_tool_call_id")
			return protocol.ModelResponse{}, fault("ID_CONFLICT")
		}
	}
	if request.Purpose == "compaction" {
		for _, part := range result.Message.Parts {
			if part.Type != "text" {
				c.state.ActiveRun.Usage.Source = "unknown"
				c.changed()
				c.mu.Unlock()
				c.failRun("invalid_compaction")
				return protocol.ModelResponse{}, fault("UNSUPPORTED_CAPABILITY")
			}
		}
	}
	for _, part := range result.Message.Parts {
		if part.Type == "tool_call" {
			c.state.UsedToolCallIDs = append(c.state.UsedToolCallIDs, part.ToolCallID)
		}
	}
	run = c.state.ActiveRun
	if result.Usage.InputTokens > math.MaxInt-run.Usage.InputTokens || result.Usage.OutputTokens > math.MaxInt-run.Usage.OutputTokens {
		run.Usage.Source = "unknown"
		c.changed()
		c.mu.Unlock()
		c.failRun("budget_unknown")
		return protocol.ModelResponse{}, fault("LIMIT_EXCEEDED")
	}
	run.Usage.InputTokens += result.Usage.InputTokens
	run.Usage.OutputTokens += result.Usage.OutputTokens
	if result.Usage.Source == "unknown" || run.Usage.Source == "unknown" {
		run.Usage.Source = "unknown"
	} else if result.Usage.Source == "estimated" {
		run.Usage.Source = "estimated"
	}
	if result.Usage.Source == "unknown" {
		for i := range c.state.PendingCalls {
			if c.state.PendingCalls[i].RequestID == request.RequestID {
				c.state.PendingCalls[i].ModelResult = &result
			}
		}
	} else {
		c.removePending(request.RequestID)
	}
	stored := StoredMessage{MessageID: "model-message:" + request.RequestID, Message: result.Message}
	events := []Event{event("call.settled", v.RunID, result)}
	if request.Purpose == "conversation" {
		c.state.Messages = append(c.state.Messages, stored)
		events = append(events, event("message.committed", v.RunID, stored))
	}
	exhausted := run.Usage.OutputTokens >= v.Limits.MaxOutputTokensTotal || result.Usage.OutputTokens > remaining.MaxOutputTokens
	overLimit := run.Usage.OutputTokens > v.Limits.MaxOutputTokensTotal || result.Usage.OutputTokens > remaining.MaxOutputTokens
	c.changed()
	c.mu.Unlock()
	if err := c.commit("call.settled", events...); err != nil {
		return protocol.ModelResponse{}, err
	}
	if result.Usage.Source == "unknown" {
		c.failRun("budget_unknown")
		return protocol.ModelResponse{}, fault("MODEL_REQUEST_FAILED")
	}
	response := protocol.ModelResponse{Turn: internalTurn(stored), Usage: protocol.Usage{InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens}, Stop: protocol.StopCompleted}
	switch result.FinishReason {
	case "tool_calls":
		response.Stop = protocol.StopToolUse
	case "length":
		response.Stop = protocol.StopLength
		c.failRun("output_limit")
	case "blocked":
		response.Stop = protocol.StopError
		c.failRun("model_blocked")
	}
	if overLimit || exhausted && response.Stop == protocol.StopToolUse {
		c.failRun("output_limit")
		response.Stop = protocol.StopLength
	}
	return response, nil
}

func (c *conversation) callback(ctx context.Context, method, id, kind string, params, result any) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.mu.Lock()
	if c.state.ActiveRun == nil || c.state.ActiveRun.Status != "running" || c.failure != nil {
		c.mu.Unlock()
		return fault("SESSION_BUSY")
	}
	c.inflight[id] = kind
	v := *c.state.RunInput
	// Enqueue while holding the same lock as interrupt admission. Once an
	// interrupt is acknowledged, no new callback can slip through this boundary.
	if ctx.Err() != nil {
		delete(c.inflight, id)
		c.mu.Unlock()
		return ctx.Err()
	}
	rpcID, replies, sendErr := c.server.p.beginCall(method, params)
	if sendErr != nil {
		delete(c.inflight, id)
		c.mu.Unlock()
		return sendErr
	}
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.inflight, id); delete(c.deltas, id); c.mu.Unlock() }()
	callCtx, cancel := context.WithCancel(c.server.p.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.server.p.awaitCall(callCtx, rpcID, replies, result) }()
	timer := time.NewTimer(time.Duration(v.Limits.CallbackTimeoutMS) * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-c.server.p.ctx.Done():
		return fault("HOST_UNAVAILABLE")
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			c.stop("elapsed_limit")
		}
	case <-timer.C:
		c.stop("callback_timeout")
	}
	// A cancellation acknowledgement is only receipt, never settlement. The
	// original RPC remains live for the bounded grace period.
	c.mu.Lock()
	reason := c.state.ActiveRun.Reason
	c.mu.Unlock()
	go func() {
		cancelCtx, release := context.WithTimeout(c.server.p.ctx, time.Duration(v.Limits.StopGraceMS)*time.Millisecond)
		defer release()
		_ = c.server.p.call(cancelCtx, "host.calls.cancel", CancelCalls{ConversationID: v.ConversationID, RunID: v.RunID, RequestIDs: []string{id}, Reason: reason}, nil)
	}()
	grace := time.NewTimer(time.Duration(v.Limits.StopGraceMS) * time.Millisecond)
	defer grace.Stop()
	select {
	case err := <-done:
		return err
	case <-grace.C:
		return fault("HOST_UNAVAILABLE")
	case <-c.server.p.ctx.Done():
		return fault("HOST_UNAVAILABLE")
	}
}

type hostPolicy struct{}

func (hostPolicy) Check(_ context.Context, r policy.Request) policy.Outcome {
	return policy.Outcome{Decision: policy.DecisionAllow, Risk: r.Risk, Reason: "forward to host authorization", Rule: "host_callback"}
}

type toolPort struct {
	c          *conversation
	definition ToolDefinition
}

func (t *toolPort) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: t.definition.Name, Description: t.definition.Description, InputSchema: t.definition.InputSchema}
}
func (*toolPort) Risk() policy.Risk        { return policy.RiskWrite }
func (*toolPort) UseExecutorTimeout() bool { return false }
func (*toolPort) Execute(context.Context, json.RawMessage) protocol.ToolResult {
	return protocol.ToolResult{IsError: true, Content: "host tool requires call identity"}
}
func (t *toolPort) ReportControl(r protocol.ToolResult) (protocol.BatchControl, protocol.CallState) {
	if v, ok := r.Metadata["host_result"].(*ToolResult); ok {
		if v.Status == "unknown" || v.Effect == "unknown" {
			return protocol.ControlInterruptRequest, protocol.CallOutcomeUnknown
		}
		switch v.Status {
		case "denied":
			return "", protocol.CallRejected
		case "cancelled":
			return "", protocol.CallCancelled
		case "failed":
			return "", protocol.CallFailed
		}
	}
	return "", ""
}
func (c *conversation) toolPrepared(call protocol.ToolCall) error {
	c.mu.Lock()
	v := *c.state.RunInput
	if c.state.ActiveRun.Status != "running" || c.failure != nil {
		c.mu.Unlock()
		return fault("SESSION_BUSY")
	}
	request := ToolRequest{ConversationID: v.ConversationID, RunID: v.RunID, RequestID: uuid.NewString(), ToolCallID: call.ID, ToolCatalog: v.ToolCatalog.Binding, TargetBinding: v.TargetBinding, Name: call.Name, Arguments: call.Arguments}
	raw, _ := json.Marshal(request)
	c.state.PendingCalls = append(c.state.PendingCalls, PendingCall{RequestID: request.RequestID, RunID: v.RunID, Kind: "tool", Digest: digest(request), Request: raw})
	c.state.ActiveRun.Phase = "waiting_host"
	c.changed()
	c.mu.Unlock()
	return c.commit("call.prepared", event("call.prepared", v.RunID, request))
}
func (t *toolPort) ExecuteCall(ctx context.Context, call protocol.ToolCall) protocol.ToolResult {
	c := t.c
	c.mu.Lock()
	var request ToolRequest
	for _, p := range c.state.PendingCalls {
		if p.Kind == "tool" {
			var r ToolRequest
			_ = json.Unmarshal(p.Request, &r)
			if r.ToolCallID == call.ID {
				request = r
				break
			}
		}
	}
	c.mu.Unlock()
	if request.RequestID == "" {
		c.stop("unprepared_tool")
		return protocol.ToolResult{IsError: true, Content: "tool was not prepared"}
	}
	result := ToolResult{RequestID: request.RequestID, Status: "unknown", Effect: "unknown", Content: []Part{}, Error: &SafeError{Code: "HOST_UNAVAILABLE", Message: "Host result requires reconciliation"}}
	compiled, err := toolSchema(t.definition.InputSchema)
	var input any
	if err == nil {
		err = json.Unmarshal(call.Arguments, &input)
	}
	if err == nil {
		err = compiled.Validate(input)
	}
	if err != nil {
		result.Status = "denied"
		result.Effect = "none"
		result.Error = &SafeError{Code: "INVALID_PARAMS", Message: "Arguments do not satisfy the supplied tool schema"}
	} else {
		var received ToolResult
		err = c.callback(ctx, "host.tool.execute", request.RequestID, "tool", request, &received)
		if err == nil {
			err = validateToolResult(received, request.RequestID)
		}
		if err == nil {
			result = received
		}
	}
	c.mu.Lock()
	if result.Status == "unknown" || result.Effect == "unknown" {
		for i := range c.state.PendingCalls {
			if c.state.PendingCalls[i].RequestID == request.RequestID {
				c.state.PendingCalls[i].ToolResult = &result
			}
		}
	} else {
		c.removePending(request.RequestID)
	}
	if result.Effect == "applied" {
		effect := request.RequestID
		if result.OperationID != nil {
			effect = *result.OperationID
		}
		c.state.ActiveRun.KnownEffects = append(c.state.ActiveRun.KnownEffects, effect)
	}
	message := StoredMessage{MessageID: "tool-message:" + request.RequestID, Message: Message{Role: "tool", Parts: []Part{{Type: "tool_result", ToolCallID: call.ID, Result: &result}}}}
	c.state.Messages = append(c.state.Messages, message)
	c.changed()
	c.mu.Unlock()
	if err := c.commit("call.settled", event("call.settled", request.RunID, result), event("message.committed", request.RunID, message)); err != nil {
		c.stop("checkpoint_failed")
	}
	if result.Status == "unknown" || result.Effect == "unknown" {
		c.stop("reconciliation_required")
	}
	return internalResult(call.ID, &result)
}

func internalResult(callID string, result *ToolResult) protocol.ToolResult {
	text := []string{}
	for _, p := range result.Content {
		text = append(text, p.Text)
	}
	return protocol.ToolResult{CallID: callID, Content: strings.Join(text, "\n"), IsError: result.Status != "succeeded", Truncated: result.Truncated, Metadata: map[string]any{"host_result": result}}
}
func internalTurn(m StoredMessage) protocol.Turn {
	role := protocol.Role(m.Message.Role)
	if role == "assistant" {
		role = protocol.RoleAgent
	}
	t := protocol.Turn{ID: m.MessageID, Role: role, CreatedAt: time.Now().UTC(), Parts: []protocol.Part{}}
	for _, p := range m.Message.Parts {
		switch p.Type {
		case "text":
			t.Parts = append(t.Parts, protocol.Part{Kind: protocol.PartText, Text: p.Text})
		case "tool_call":
			t.Parts = append(t.Parts, protocol.Part{Kind: protocol.PartToolCall, ToolCall: &protocol.ToolCall{ID: p.ToolCallID, Name: p.Name, Arguments: p.Arguments}})
		case "tool_result":
			r := internalResult(p.ToolCallID, p.Result)
			t.Parts = append(t.Parts, protocol.Part{Kind: protocol.PartToolResult, ToolResult: &r})
		}
	}
	return t
}
func storedTurn(t protocol.Turn) StoredMessage {
	role := string(t.Role)
	if t.Role == protocol.RoleAgent {
		role = "assistant"
	}
	m := StoredMessage{MessageID: t.ID, Message: Message{Role: role, Parts: []Part{}}}
	for _, p := range t.Parts {
		switch p.Kind {
		case protocol.PartText:
			m.Message.Parts = append(m.Message.Parts, Part{Type: "text", Text: p.Text})
		case protocol.PartToolCall:
			if p.ToolCall != nil {
				m.Message.Parts = append(m.Message.Parts, Part{Type: "tool_call", ToolCallID: p.ToolCall.ID, Name: p.ToolCall.Name, Arguments: p.ToolCall.Arguments})
			}
		case protocol.PartToolResult:
			if p.ToolResult == nil {
				continue
			}
			r := p.ToolResult
			result, ok := r.Metadata["host_result"].(*ToolResult)
			if !ok {
				result = notDispatched(r.CallID, r.Content)
			}
			m.Message.Parts = append(m.Message.Parts, Part{Type: "tool_result", ToolCallID: r.CallID, Result: result})
		}
	}
	return m
}
func restoreAgent(state State) (*agent.Conversation, error) {
	turns := []protocol.Turn{}
	for _, m := range state.Messages {
		turns = append(turns, internalTurn(m))
	}
	return agent.RestoreConversationForProfile(agent.ConversationState{SessionID: state.ConversationID, Turns: turns, Summary: state.Summary}, agent.Profile{Name: "host", PermissionMode: "host", SystemPrompt: func() string { return state.Instructions }})
}
