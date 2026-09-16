package agent

import (
	"fmt"
	"sync"
	"time"

	"Eylu/internal/protocol"
)

// streamDeltaFlushBytes bounds how much streamed text may be coalesced before it
// is delivered, so the host keeps receiving progress while a burst of deltas does
// not stall the model.
const streamDeltaFlushBytes = 4 << 10

// streamDeltaSlowThreshold is how long one delivery may take before the host is
// treated as a slow consumer. It bounds what the model stream pays for a host
// that cannot keep up: once a delivery overruns the budget, streamed text stops
// being pushed through the sink.
const streamDeltaSlowThreshold = 250 * time.Millisecond

// eventQueue delivers host events in order while coalescing streamed text.
//
// Delivery is classified, and the two classes have different contracts:
//
//   - A critical event - a tool starting or finishing, a usage or terminal
//     report, anything that carries control or durable state - is delivered
//     synchronously, in order, and its failure stops the request. It can never be
//     dropped.
//   - A streamed text or reasoning delta is progress, not state: its content is
//     the concatenation of the pieces and the transcript holds the answer either
//     way. It is coalesced, and while the host is behind it is dropped and
//     counted instead of holding the model's stream open.
//
// The queue holds no goroutine and no state that outlives one request, so every
// host callback still runs on the request goroutine with the conversation state
// lock released.
type eventQueue struct {
	mu      sync.Mutex
	sink    func(protocol.ModelEvent) error
	pending *protocol.ModelEvent

	// slowThreshold is the delivery budget. A test may shorten it.
	slowThreshold time.Duration
	// now is the clock used to measure a delivery, so a case can exercise the
	// budget without sleeping.
	now func() time.Time
	// slow reports that the host could not keep up with the previous delivery.
	slow bool
	// dropped counts the streamed pieces that were coalesced away while the host
	// was behind.
	dropped int
	// slowNote records the first slow delivery, for the run report.
	slowNote string
}

func startEventQueue(sink func(protocol.ModelEvent) error) *eventQueue {
	return &eventQueue{sink: sink, slowThreshold: streamDeltaSlowThreshold, now: time.Now}
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
		if q.slow {
			// The host is behind, so holding the text back no longer bounds what
			// the model stream pays for. The piece is dropped and counted; the
			// transcript still holds the answer, and the count says the host's
			// view of the stream is incomplete.
			q.dropped++
			return nil
		}
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
	return q.deliverLocked(event)
}

// stop flushes the pending text and ends the queue. It is called on every exit
// path of a request, so no streamed content is left in the buffer.
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
	return q.deliverLocked(event)
}

// deliverLocked calls the host and measures how long it took.
//
// The measurement is what makes a slow consumer visible: a delivery that overruns
// the budget marks the host as behind, and a delivery that fits marks it as caught
// up again. Nothing is dropped here - the caller decides whether the event may be
// dropped, and a critical event never may.
func (q *eventQueue) deliverLocked(event protocol.ModelEvent) error {
	started := q.clock()
	err := q.sink(event)
	elapsed := q.clock().Sub(started)
	if elapsed >= q.slowThreshold {
		if q.slowNote == "" {
			q.slowNote = fmt.Sprintf("the host event consumer is slower than %s (one delivery took %s)", q.slowThreshold, elapsed)
		}
		q.slow = true
	} else {
		q.slow = false
	}
	return err
}

func (q *eventQueue) clock() time.Time {
	if q.now == nil {
		return time.Now()
	}
	return q.now()
}

// droppedCount reports how many streamed pieces were not delivered.
func (q *eventQueue) droppedCount() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

// slowDiagnostic reports the first slow delivery, or an empty string.
func (q *eventQueue) slowDiagnostic() string {
	if q == nil {
		return ""
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.slowNote
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
