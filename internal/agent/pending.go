package agent

import (
	"Eylu/internal/protocol"
)

// PendingCall is one committed tool call in the explicit set a Conversation keeps
// for the duration of a request.
//
// The set is the single source of truth for "which calls are still open". A call
// enters it when its turn is committed, is marked prepared when the executor
// accepts it, and carries a terminal state once a result is recorded. Closing a
// request therefore consumes this set instead of re-deriving it by scanning the
// transcript, so the closure of the history and the count the run report publishes
// cannot disagree about what was still open.
//
// The fields are deliberately the ones an operator needs to answer "what is
// outstanding, and where did it come from?" without reading the transcript.
type PendingCall struct {
	CallID       string `json:"call_id"`
	Tool         string `json:"tool"`
	ParentCallID string `json:"parent_call_id,omitempty"`
	// TurnID is the turn the call was committed in, Iteration is the round of the
	// request it belongs to, and RequestID is the request itself. The executor's
	// own RequestID is the same value, so the two records can be joined.
	TurnID    string `json:"turn_id"`
	Iteration int    `json:"iteration"`
	// RequestID is the request the call belongs to, and is the same value the
	// executor sees, so the log's records and this view can be joined.
	RequestID string `json:"request_id"`
	// Prepared reports that the executor accepted the call for execution. Until it
	// is set the call provably has not run.
	Prepared bool `json:"prepared"`
	// State is the terminal state, and stays empty while the call is open.
	State protocol.CallState `json:"state,omitempty"`
}

// trackCommittedCalls adds the calls of one committed turn to the pending set.
// It is called under the state lock, immediately after the turn is committed, so
// the set describes the transcript rather than a later view of it.
func (c *Conversation) trackCommittedCalls(requestID string, iteration int, turn protocol.Turn) {
	for _, call := range toolCalls(turn) {
		if call.ID == "" {
			continue
		}
		if entry := c.pendingEntryLocked(call.ID); entry != nil {
			// A call ID is host-visible identity: the same ID must not describe two
			// different calls, so a repeat is left as the first one recorded.
			continue
		}
		c.pendingCalls = append(c.pendingCalls, PendingCall{
			CallID: call.ID, Tool: call.Name, ParentCallID: call.ParentCallID,
			TurnID: turn.ID, Iteration: iteration, RequestID: requestID,
		})
	}
}

// markCallPrepared records that the executor accepted one call for execution.
// After this point the call may have had an effect, and before it, it provably
// has not.
func (c *Conversation) markCallPrepared(callID string) {
	if callID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.pendingEntryLocked(callID); entry != nil {
		entry.Prepared = true
	}
}

// pendingEntryLocked returns the entry of one call, or nil. The caller holds the
// state lock.
func (c *Conversation) pendingEntryLocked(callID string) *PendingCall {
	for index := range c.pendingCalls {
		if c.pendingCalls[index].CallID == callID {
			return &c.pendingCalls[index]
		}
	}
	return nil
}

// openPendingLocked returns the calls of the set that have no terminal state.
func (c *Conversation) openPendingLocked() []PendingCall {
	open := make([]PendingCall, 0, len(c.pendingCalls))
	for _, entry := range c.pendingCalls {
		if entry.State == "" {
			open = append(open, entry)
		}
	}
	return open
}

// PendingCalls reports the calls of the request that is running, or of the most
// recent one, including the ones that already reached a terminal state.
//
// An empty result is the healthy case: a request that ends with anything still
// open is a defect, and the run report publishes that count so it cannot pass
// unnoticed.
func (c *Conversation) PendingCalls() []PendingCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]PendingCall(nil), c.pendingCalls...)
}

// OpenPendingCalls reports the calls that have no terminal state yet.
func (c *Conversation) OpenPendingCalls() []PendingCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.openPendingLocked()
}

// forgetPendingCalls clears the set. A request owns the set for its whole life, so
// it is emptied when the request ends: the next request describes its own calls.
func (c *Conversation) forgetPendingCalls() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pendingCalls = nil
}
