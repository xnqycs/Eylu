package agent

import (
	"sync"

	"Eylu/internal/protocol"
)

// streamDeltaFlushBytes bounds how much streamed text may be coalesced before it
// is delivered, so the host keeps receiving progress while a burst of deltas does
// not stall the model.
const streamDeltaFlushBytes = 4 << 10

// eventQueue delivers host events in order while coalescing streamed text.
//
// It is deliberately not an asynchronous pipe: a terminal, control or content
// event is delivered synchronously, so its failure stops the request immediately
// and it can never be dropped in a buffer. Only streamed text and reasoning
// deltas are buffered, because their content is the concatenation of the pieces
// and the rendered answer is identical either way; they are flushed when the
// buffer reaches its bound, when any other event arrives, and when the request
// ends.
//
// The queue holds no goroutine and no state that outlives one request, so every
// host callback still runs on the request goroutine with the conversation state
// lock released.
type eventQueue struct {
	mu      sync.Mutex
	sink    func(protocol.ModelEvent) error
	pending *protocol.ModelEvent
}

func startEventQueue(sink func(protocol.ModelEvent) error) *eventQueue {
	return &eventQueue{sink: sink}
}

// push delivers one event, coalescing a streamed delta into the pending one when
// it can.
func (q *eventQueue) push(event protocol.ModelEvent) error {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if isStreamedDelta(event.Kind) {
		if q.pending != nil && q.pending.Kind == event.Kind && len(q.pending.Delta) < streamDeltaFlushBytes {
			q.pending.Delta += event.Delta
			if len(q.pending.Delta) < streamDeltaFlushBytes {
				return nil
			}
			return q.flushLocked()
		}
		if err := q.flushLocked(); err != nil {
			return err
		}
		merged := event
		q.pending = &merged
		if len(merged.Delta) < streamDeltaFlushBytes {
			return nil
		}
		return q.flushLocked()
	}
	// Any other event is delivered after the text that preceded it, in order.
	if err := q.flushLocked(); err != nil {
		return err
	}
	return q.sink(event)
}

// stop flushes the pending text and ends the queue. It is called on every exit
// path of a request, so no streamed content is lost when a request ends.
func (q *eventQueue) stop() error {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.flushLocked()
}

func (q *eventQueue) flushLocked() error {
	if q.pending == nil {
		return nil
	}
	event := *q.pending
	q.pending = nil
	return q.sink(event)
}

// isStreamedDelta reports whether an event carries a piece of streamed text whose
// value to the host is the concatenation of the pieces.
func isStreamedDelta(kind protocol.EventKind) bool {
	switch kind {
	case protocol.EventTextDelta, protocol.EventReasoningDelta:
		return true
	default:
		return false
	}
}
