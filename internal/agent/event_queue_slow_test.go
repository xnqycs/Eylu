package agent

import (
	"errors"
	"strings"
	"testing"
	"time"

	"Eylu/internal/protocol"
)

// errCritical is the failure a critical event delivery reports.
var errCritical = errors.New("event sink is gone")

// steppingClock advances by a fixed amount on every reading, so a case can
// exercise the delivery budget without sleeping through it.
type steppingClock struct {
	step time.Duration
	now  time.Time
}

func (c *steppingClock) Now() time.Time {
	c.now = c.now.Add(c.step)
	return c.now
}

// A host that cannot keep up does not hold the model stream open. Critical events
// still arrive, in order; the streamed text that could not be delivered is
// dropped and counted, and the consumer is diagnosed once.
func TestSlowEventConsumerKeepsCriticalEventsAndCountsDroppedDeltas(t *testing.T) {
	var delivered []protocol.ModelEvent
	queue := startEventQueue(func(event protocol.ModelEvent) error {
		delivered = append(delivered, event)
		return nil
	})
	queue.slowThreshold = 100 * time.Millisecond
	clock := &steppingClock{step: time.Second}
	queue.now = clock.Now

	// A full buffer per push forces one delivery per call, which is what lets the
	// budget observe the consumer.
	delta := strings.Repeat("x", streamDeltaFlushBytes)
	for index := 0; index < 4; index++ {
		if err := queue.push(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: delta}); err != nil {
			t.Fatalf("push %d: %v", index, err)
		}
	}
	if err := queue.push(protocol.ModelEvent{Kind: protocol.EventToolStart}); err != nil {
		t.Fatalf("push critical: %v", err)
	}
	if err := queue.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if queue.droppedCount() != 3 {
		t.Fatalf("dropped = %d, want the three pieces the host was too slow to take", queue.droppedCount())
	}
	if queue.slowDiagnostic() == "" {
		t.Fatal("the slow consumer was not diagnosed")
	}
	if len(delivered) == 0 || delivered[len(delivered)-1].Kind != protocol.EventToolStart {
		t.Fatalf("delivered = %#v, want the critical event last", delivered)
	}
	for _, event := range delivered {
		if event.Kind != protocol.EventTextDelta && event.Kind != protocol.EventToolStart {
			t.Fatalf("unexpected event %#v", event)
		}
	}
}

// A host that keeps up loses nothing, and coalescing still applies.
func TestFastEventConsumerDropsNothing(t *testing.T) {
	var delivered []protocol.ModelEvent
	queue := startEventQueue(func(event protocol.ModelEvent) error {
		delivered = append(delivered, event)
		return nil
	})
	queue.slowThreshold = time.Hour

	if err := queue.push(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := queue.push(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: "two"}); err != nil {
		t.Fatal(err)
	}
	if err := queue.push(protocol.ModelEvent{Kind: protocol.EventToolStart}); err != nil {
		t.Fatal(err)
	}
	if err := queue.stop(); err != nil {
		t.Fatal(err)
	}
	if queue.droppedCount() != 0 {
		t.Fatalf("dropped = %d, want none", queue.droppedCount())
	}
	if queue.slowDiagnostic() != "" {
		t.Fatalf("a fast consumer was diagnosed as slow: %q", queue.slowDiagnostic())
	}
	// The two pieces of one delta kind are coalesced into one delivery, which
	// arrives before the critical event.
	if len(delivered) != 2 || delivered[0].Delta != "onetwo" || delivered[1].Kind != protocol.EventToolStart {
		t.Fatalf("delivered = %#v", delivered)
	}
}

// The failure of a critical event is still the caller's to act on: the queue
// reports it instead of swallowing it.
func TestCriticalEventFailureIsReported(t *testing.T) {
	failure := protocol.ModelEvent{Kind: protocol.EventToolStart}
	queue := startEventQueue(func(protocol.ModelEvent) error { return errCritical })
	if err := queue.push(failure); err == nil {
		t.Fatal("a critical delivery failure was swallowed")
	}
	// A streamed delta is progress, so its failure is reported too but never
	// dropped silently at the source.
	var seen int
	streamed := startEventQueue(func(protocol.ModelEvent) error { seen++; return nil })
	streamed.slowThreshold = time.Hour
	if err := streamed.push(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: strings.Repeat("y", streamDeltaFlushBytes)}); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Fatalf("streamed deliveries = %d", seen)
	}
}
