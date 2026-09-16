package testfault

import (
	"fmt"
	"sync"
)

// Schedule decides which calls of one dependency fail. Calls are counted per
// Fault, starting at 1.
type Schedule func(call int) bool

// Never is the schedule of a dependency that always works.
func Never() Schedule { return func(int) bool { return false } }

// Always is "持续失败": every call fails.
func Always() Schedule { return func(int) bool { return true } }

// Nth is "第 N 次调用失败": only the n-th call fails.
func Nth(n int) Schedule {
	return func(call int) bool { return call == n }
}

// FirstN is "失败后恢复": the first n calls fail and every later call succeeds.
func FirstN(n int) Schedule {
	return func(call int) bool { return call <= n }
}

// AfterN fails every call after the n-th, which is "the dependency broke for
// good part way through".
func AfterN(n int) Schedule {
	return func(call int) bool { return call > n }
}

// Between fails the calls from `from` to `to`, inclusive.
func Between(from, to int) Schedule {
	return func(call int) bool { return call >= from && call <= to }
}

// Any fails when at least one of the schedules fails. It combines independent
// faults on the same dependency.
func Any(schedules ...Schedule) Schedule {
	return func(call int) bool {
		for _, schedule := range schedules {
			if schedule != nil && schedule(call) {
				return true
			}
		}
		return false
	}
}

// All fails only when every schedule fails, and never for an empty list.
func All(schedules ...Schedule) Schedule {
	return func(call int) bool {
		if len(schedules) == 0 {
			return false
		}
		for _, schedule := range schedules {
			if schedule == nil || !schedule(call) {
				return false
			}
		}
		return true
	}
}

// Fault counts the calls of one dependency and applies a Schedule to them. It is
// the single place a test decides when a host dependency breaks, so the same
// three modes are available on every injection surface.
//
// A nil *Fault never fails and reports zero counts, so an injection surface can
// leave the faults it does not use unset.
type Fault struct {
	name string
	plan Schedule

	mu    sync.Mutex
	calls int
	fails int
}

// NewFault returns a fault named for the dependency it guards. The name appears
// in the injected error, so a test failure says which dependency broke.
func NewFault(name string, plan Schedule) *Fault {
	if plan == nil {
		plan = Never()
	}
	return &Fault{name: name, plan: plan}
}

// Fail counts one call and returns the error the caller must report, or nil when
// this call is allowed through.
func (f *Fault) Fail() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.plan == nil || !f.plan(f.calls) {
		return nil
	}
	f.fails++
	return fmt.Errorf("%s: injected failure on call %d", f.name, f.calls)
}

// Calls reports how many calls reached the fault.
func (f *Fault) Calls() int {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Failures reports how many of those calls were failed on purpose.
func (f *Fault) Failures() int {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fails
}
