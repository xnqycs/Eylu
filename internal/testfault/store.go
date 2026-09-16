package testfault

import (
	"errors"

	"Eylu/internal/session"
)

// Store is the explicit form of the two durable seams a session runtime depends
// on: the append-only event log and the snapshot checkpoint. The production
// runtime holds exactly these two operations, and this interface names them so
// one object can fail both halves instead of two closures a test has to keep in
// step.
type Store interface {
	Append(id string, events []session.Event) ([]session.Event, error)
	Save(snapshot session.Snapshot) error
}

// StoreFaults forwards to a real store and fails either half on schedule.
//
// A nil Delegate is not an error: it is how a test asserts that the call must
// not reach the store at all.
type StoreFaults struct {
	Delegate    Store
	AppendFault *Fault
	SaveFault   *Fault
}

var _ Store = (*StoreFaults)(nil)

// Append records the events in the real store unless the schedule fails first.
func (s *StoreFaults) Append(id string, events []session.Event) ([]session.Event, error) {
	if err := s.AppendFault.Fail(); err != nil {
		return nil, err
	}
	if s.Delegate == nil {
		return nil, errors.New("testfault: append reached a store with no delegate")
	}
	return s.Delegate.Append(id, events)
}

// Save writes the snapshot in the real store unless the schedule fails first.
func (s *StoreFaults) Save(snapshot session.Snapshot) error {
	if err := s.SaveFault.Fail(); err != nil {
		return err
	}
	if s.Delegate == nil {
		return errors.New("testfault: save reached a store with no delegate")
	}
	return s.Delegate.Save(snapshot)
}
