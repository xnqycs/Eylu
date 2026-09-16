package testfault

import (
	"sync"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

// EventFaults wraps the host event consumer, which is the driver.EmitFunc the
// request is run with. It fails delivery on schedule and can hold every delivery
// until the test releases it.
type EventFaults struct {
	Delegate driver.EmitFunc
	Fault    *Fault
	// Hold, when non-nil, makes every delivery wait until it is closed. It is the
	// barrier form of a slow consumer: the test decides exactly when the host
	// resumes, so a case never depends on a sleep to hit the window it wants.
	Hold <-chan struct{}

	mu     sync.Mutex
	events []protocol.ModelEvent
}

var _ driver.EmitFunc = (&EventFaults{}).Emit

// Emit keeps the event, waits for the barrier when one is set, and then delivers
// it to the delegate unless the schedule fails first.
func (e *EventFaults) Emit(event protocol.ModelEvent) error {
	e.mu.Lock()
	e.events = append(e.events, event)
	e.mu.Unlock()
	if e.Hold != nil {
		<-e.Hold
	}
	if err := e.Fault.Fail(); err != nil {
		return err
	}
	if e.Delegate == nil {
		return nil
	}
	return e.Delegate(event)
}

// Events returns every event the host was offered, including the ones whose
// delivery failed.
func (e *EventFaults) Events() []protocol.ModelEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]protocol.ModelEvent(nil), e.events...)
}

// KindCount reports how many events of one kind the host was offered,
// including the ones whose delivery failed.
func (e *EventFaults) KindCount(kind protocol.EventKind) int {
	count := 0
	for _, event := range e.Events() {
		if event.Kind == kind {
			count++
		}
	}
	return count
}
