package host

import (
	"encoding/json"
	"slices"
)

func validateState(v Restore) error {
	s := v.State
	if v.StateSchema != StateSchema || v.Revision < 1 || v.EventSeq < 1 || s.Revision != v.Revision || s.EventSeq != v.EventSeq || s.ConversationID != v.ConversationID || !validCreate(Create{ConversationID: s.ConversationID, Instructions: s.Instructions, Context: s.Context}) || s.CreateDigest != digest(Create{ConversationID: s.ConversationID, Instructions: s.Instructions, Context: s.Context}) {
		return fault("STATE_INCOMPATIBLE")
	}
	if s.RunInput != nil {
		if s.RunInput.ConversationID != s.ConversationID || validateStart(*s.RunInput) != nil {
			return fault("STATE_INCOMPATIBLE")
		}
	}
	seen := map[string]bool{}
	calls := map[string]bool{}
	results := map[string]bool{}
	for _, m := range s.Messages {
		if !identifier(m.MessageID) || seen[m.MessageID] || validateMessage(m.Message) != nil {
			return fault("STATE_INCOMPATIBLE")
		}
		seen[m.MessageID] = true
		for _, p := range m.Message.Parts {
			if p.Type == "tool_call" {
				if calls[p.ToolCallID] || !slices.Contains(s.UsedToolCallIDs, p.ToolCallID) {
					return fault("STATE_INCOMPATIBLE")
				}
				calls[p.ToolCallID] = true
			}
			if p.Type == "tool_result" {
				if !calls[p.ToolCallID] || results[p.ToolCallID] {
					return fault("STATE_INCOMPATIBLE")
				}
				results[p.ToolCallID] = true
			}
		}
	}
	seen = map[string]bool{}
	for _, id := range s.UsedToolCallIDs {
		if !identifier(id) || seen[id] {
			return fault("STATE_INCOMPATIBLE")
		}
		seen[id] = true
	}
	for id, r := range s.Runs {
		if !identifier(id) || r.Accepted.ConversationID != s.ConversationID || r.Accepted.RunID != id || r.Accepted.Revision < 2 || r.Accepted.Revision > v.Revision || len(r.Digest) != 64 {
			return fault("STATE_INCOMPATIBLE")
		}
	}
	for _, r := range []*Run{s.ActiveRun, s.LastRun} {
		if r == nil {
			continue
		}
		if !validUsage(r.Usage) || r.ModelCalls < 0 || !slices.Contains([]string{"running", "stopping", "completed", "interrupted", "failed"}, r.Status) {
			return fault("STATE_INCOMPATIBLE")
		}
		if _, ok := s.Runs[r.RunID]; !ok {
			return fault("STATE_INCOMPATIBLE")
		}
	}
	if s.ActiveRun != nil {
		if !slices.Contains([]string{"running", "stopping"}, s.ActiveRun.Status) || !slices.Contains([]string{"preparing", "waiting_model", "waiting_host", "committing"}, s.ActiveRun.Phase) {
			return fault("STATE_INCOMPATIBLE")
		}
	}
	if s.LastRun != nil && !slices.Contains([]string{"completed", "interrupted", "failed"}, s.LastRun.Status) {
		return fault("STATE_INCOMPATIBLE")
	}
	owner := s.ActiveRun
	if owner == nil {
		owner = s.LastRun
	}
	seen = map[string]bool{}
	for _, p := range s.PendingCalls {
		if !identifier(p.RequestID) || seen[p.RequestID] || owner == nil || owner.RunID != p.RunID || s.RunInput == nil || s.RunInput.RunID != p.RunID || p.Digest != digest(p.Request) {
			return fault("STATE_INCOMPATIBLE")
		}
		seen[p.RequestID] = true
		switch p.Kind {
		case "model":
			var r ModelRequest
			if decode(p.Request, &r) != nil || r.RequestID != p.RequestID || r.ConversationID != s.ConversationID || r.RunID != p.RunID || digest(r.ModelBinding) != digest(s.RunInput.ModelBinding) || !slices.Contains([]string{"conversation", "compaction"}, r.Purpose) || p.ToolResult != nil {
				return fault("STATE_INCOMPATIBLE")
			}
			if p.ModelResult != nil && validateModelResult(*p.ModelResult, p.RequestID) != nil {
				return fault("STATE_INCOMPATIBLE")
			}
		case "tool":
			var r ToolRequest
			if decode(p.Request, &r) != nil || r.RequestID != p.RequestID || r.ConversationID != s.ConversationID || r.RunID != p.RunID || r.ToolCatalog != s.RunInput.ToolCatalog.Binding || r.TargetBinding != s.RunInput.TargetBinding || !calls[r.ToolCallID] || p.ModelResult != nil {
				return fault("STATE_INCOMPATIBLE")
			}
			if p.ToolResult != nil && validateToolResult(*p.ToolResult, p.RequestID) != nil {
				return fault("STATE_INCOMPATIBLE")
			}
		default:
			return fault("STATE_INCOMPATIBLE")
		}
	}
	return nil
}
func (s *Server) restore(v Restore) (any, *RPCError) {
	if validateState(v) != nil {
		return nil, fault("STATE_INCOMPATIBLE")
	}
	evidence := map[string]Reconciliation{}
	for _, r := range v.Reconciliations {
		if r.ModelResult != nil {
			normalized := normalizeStoppedModel(*r.ModelResult)
			r.ModelResult = &normalized
		}
		if _, exists := evidence[r.RequestID]; exists {
			return nil, fault("INVALID_PARAMS")
		}
		index := slices.IndexFunc(v.State.PendingCalls, func(p PendingCall) bool { return p.RequestID == r.RequestID })
		if index < 0 || !slices.Contains([]string{"not_dispatched", "settled", "unknown"}, r.Status) {
			return nil, fault("INVALID_PARAMS")
		}
		p := v.State.PendingCalls[index]
		if r.Status != "settled" && (r.ModelResult != nil || r.ToolResult != nil) {
			return nil, fault("INVALID_PARAMS")
		}
		if r.Status == "not_dispatched" && (p.ModelResult != nil || p.ToolResult != nil) {
			return nil, fault("ID_CONFLICT")
		}
		if r.Status == "settled" {
			if p.Kind == "model" && (r.ToolResult != nil || r.ModelResult == nil || validateModelResult(*r.ModelResult, p.RequestID) != nil) {
				return nil, fault("INVALID_PARAMS")
			}
			if p.Kind == "tool" && (r.ModelResult != nil || r.ToolResult == nil || validateToolResult(*r.ToolResult, p.RequestID) != nil) {
				return nil, fault("INVALID_PARAMS")
			}
			if p.ModelResult != nil && r.ModelResult != nil && digest(p.ModelResult.Message) != digest(r.ModelResult.Message) {
				return nil, fault("ID_CONFLICT")
			}
			if p.ToolResult != nil && r.ToolResult != nil {
				if p.ToolResult.OperationID != nil && (r.ToolResult.OperationID == nil || *p.ToolResult.OperationID != *r.ToolResult.OperationID) {
					return nil, fault("ID_CONFLICT")
				}
				if p.ToolResult.Effect == "applied" && r.ToolResult.Effect == "none" {
					return nil, fault("ID_CONFLICT")
				}
			}
		}
		evidence[r.RequestID] = r
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, fault("SESSION_BUSY")
	}
	if old := s.sessions[v.ConversationID]; old != nil {
		old.mu.Lock()
		if old.done != nil {
			select {
			case <-old.done:
			default:
				old.mu.Unlock()
				s.mu.Unlock()
				return nil, fault("SESSION_BUSY")
			}
		}
		if v.Revision < old.revision {
			old.mu.Unlock()
			s.mu.Unlock()
			return nil, fault("STALE_REVISION")
		}
		if v.Revision == old.revision && digest(v.State) != digest(old.saved) {
			old.mu.Unlock()
			s.mu.Unlock()
			return nil, fault("ID_CONFLICT")
		}
		if old.revision == 0 && old.failure == nil {
			old.mu.Unlock()
			s.mu.Unlock()
			return nil, fault("SESSION_BUSY")
		}
		old.mu.Unlock()
	}
	c := newConversation(s, clone(v.State))
	s.sessions[v.ConversationID] = c
	// Reserve a writer until reconciliation and its checkpoint finish.
	c.done = make(chan struct{})
	c.mu.Lock()
	s.mu.Unlock()
	defer close(c.done)
	if c.state.ActiveRun == nil && len(c.state.PendingCalls) > 0 {
		c.state.ActiveRun = clone(c.state.LastRun)
		c.state.LastRun = nil
	}
	if c.state.ActiveRun == nil {
		c.mu.Unlock()
		return c.view(), nil
	}
	active := c.state.ActiveRun
	active.Status = "stopping"
	active.Phase = "preparing"
	active.Reason = "restored_interrupted"
	active.BlockingReason = "reconciliation_required"
	remaining := []PendingCall{}
	events := []Event{}
	for _, p := range c.state.PendingCalls {
		r, ok := evidence[p.RequestID]
		if !ok || r.Status == "unknown" {
			remaining = append(remaining, p)
			continue
		}
		if r.Status == "settled" {
			if p.Kind == "model" {
				result := *r.ModelResult
				if p.ModelResult != nil {
					active.Usage.InputTokens -= p.ModelResult.Usage.InputTokens
					active.Usage.OutputTokens -= p.ModelResult.Usage.OutputTokens
				}
				active.Usage.InputTokens += result.Usage.InputTokens
				active.Usage.OutputTokens += result.Usage.OutputTokens
				active.Usage.Source = result.Usage.Source
				var request ModelRequest
				_ = json.Unmarshal(p.Request, &request)
				if request.Purpose == "conversation" {
					m := StoredMessage{MessageID: "model-message:" + p.RequestID, Message: result.Message}
					c.putMessage(m)
					events = append(events, event("message.committed", p.RunID, m))
					for _, part := range result.Message.Parts {
						if part.Type == "tool_call" && !slices.Contains(c.state.UsedToolCallIDs, part.ToolCallID) {
							c.state.UsedToolCallIDs = append(c.state.UsedToolCallIDs, part.ToolCallID)
						}
					}
				}
				if result.Usage.Source == "unknown" {
					p.ModelResult = &result
					remaining = append(remaining, p)
				}
			} else {
				result := *r.ToolResult
				var request ToolRequest
				_ = json.Unmarshal(p.Request, &request)
				m := StoredMessage{MessageID: "tool-message:" + p.RequestID, Message: Message{Role: "tool", Parts: []Part{{Type: "tool_result", ToolCallID: request.ToolCallID, Result: &result}}}}
				c.putMessage(m)
				events = append(events, event("message.committed", p.RunID, m))
				if result.Effect == "applied" {
					id := p.RequestID
					if result.OperationID != nil {
						id = *result.OperationID
					}
					if !slices.Contains(active.KnownEffects, id) {
						active.KnownEffects = append(active.KnownEffects, id)
					}
				}
				if result.Status == "unknown" || result.Effect == "unknown" {
					p.ToolResult = &result
					remaining = append(remaining, p)
				}
			}
		}
		events = append(events, event("call.settled", p.RunID, r))
	}
	c.state.PendingCalls = remaining
	active.UnresolvedRequestIDs = []string{}
	for _, p := range remaining {
		active.UnresolvedRequestIDs = append(active.UnresolvedRequestIDs, p.RequestID)
	}
	if len(remaining) == 0 {
		// Close every never-dispatched request from a committed model response.
		for _, m := range append([]StoredMessage{}, c.state.Messages...) {
			for _, p := range m.Message.Parts {
				if p.Type == "tool_call" && !hasToolResult(c.state.Messages, p.ToolCallID) {
					result := notDispatched(p.ToolCallID, "Old run ended during recovery; this tool was not dispatched")
					closed := StoredMessage{MessageID: "closed:" + digest(p.ToolCallID), Message: Message{Role: "tool", Parts: []Part{{Type: "tool_result", ToolCallID: p.ToolCallID, Result: result}}}}
					c.state.Messages = append(c.state.Messages, closed)
					events = append(events, event("message.committed", active.RunID, closed))
				}
			}
		}
		active.Status = "interrupted"
		active.Phase = ""
		active.BlockingReason = ""
		c.state.LastRun = active
		c.state.ActiveRun = nil
		record := c.state.Runs[active.RunID]
		record.Outcome = clone(active)
		c.state.Runs[active.RunID] = record
		events = append(events, event("run.finished", active.RunID, active))
	}
	c.changed()
	c.mu.Unlock()
	if err := c.commit("conversation.restored", events...); err != nil {
		return nil, asFault(err, "CHECKPOINT_FAILED")
	}
	return c.view(), nil
}
func (c *conversation) putMessage(m StoredMessage) {
	for i, old := range c.state.Messages {
		if old.MessageID == m.MessageID {
			c.state.Messages[i] = m
			return
		}
	}
	c.state.Messages = append(c.state.Messages, m)
}
func notDispatched(id, text string) *ToolResult {
	return &ToolResult{RequestID: "not-dispatched:" + digest(id), Status: "cancelled", Effect: "none", Content: []Part{{Type: "text", Text: text}}, Error: &SafeError{Code: "NOT_DISPATCHED", Message: "Tool call was not dispatched"}}
}
