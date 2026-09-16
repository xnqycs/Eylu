package testfault

import (
	"strings"
	"testing"

	"Eylu/internal/session"
)

// The three modes the hardening plan asks for are schedules, and the composing
// helpers keep them independent of one another.
func TestSchedulesCoverTheThreeRequiredModes(t *testing.T) {
	cases := []struct {
		name string
		plan Schedule
		want []bool
	}{
		{"never", Never(), []bool{false, false, false, false, false}},
		{"always (持续失败)", Always(), []bool{true, true, true, true, true}},
		{"nth (第 N 次调用失败)", Nth(2), []bool{false, true, false, false, false}},
		{"firstN (失败后恢复)", FirstN(2), []bool{true, true, false, false, false}},
		{"afterN", AfterN(3), []bool{false, false, false, true, true}},
		{"between", Between(2, 3), []bool{false, true, true, false, false}},
		{"any", Any(Nth(1), AfterN(4)), []bool{true, false, false, false, true}},
		{"all", All(AfterN(1), FirstN(4)), []bool{false, true, true, true, false}},
		{"all of nothing", All(), []bool{false, false, false, false, false}},
		{"any with an unset schedule", Any(nil, Nth(1)), []bool{true, false, false, false, false}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			for index, want := range testCase.want {
				if got := testCase.plan(index + 1); got != want {
					t.Fatalf("call %d = %v, want %v", index+1, got, want)
				}
			}
		})
	}
}

// A fault counts every call, names the dependency it guards, and recovers when
// its schedule says so.
func TestFaultCountsCallsAndFailures(t *testing.T) {
	fault := NewFault("store.Append", FirstN(2))
	for call := 1; call <= 4; call++ {
		err := fault.Fail()
		switch {
		case call <= 2 && err == nil:
			t.Fatalf("call %d did not fail", call)
		case call <= 2 && !strings.Contains(err.Error(), "store.Append"):
			t.Fatalf("call %d error = %v, want the dependency name", call, err)
		case call > 2 && err != nil:
			t.Fatalf("call %d failed after the schedule recovered: %v", call, err)
		}
	}
	if fault.Calls() != 4 {
		t.Fatalf("calls = %d, want 4", fault.Calls())
	}
	if fault.Failures() != 2 {
		t.Fatalf("failures = %d, want 2", fault.Failures())
	}
}

// An unset fault is how an injection surface says "this half is not being
// faulted", so it must be usable without a nil check at the call site.
func TestUnsetFaultsAndSchedulesNeverFail(t *testing.T) {
	var unset *Fault
	if err := unset.Fail(); err != nil {
		t.Fatalf("an unset fault failed: %v", err)
	}
	if unset.Calls() != 0 || unset.Failures() != 0 {
		t.Fatalf("an unset fault counted calls = %d failures = %d", unset.Calls(), unset.Failures())
	}
	if err := NewFault("empty", nil).Fail(); err != nil {
		t.Fatalf("a fault with no schedule failed: %v", err)
	}
}

// The store surface fails before it reaches the delegate, and says so when a
// test forgot to wire one while no fault was scheduled.
func TestStoreFaultsFailBeforeReachingTheDelegate(t *testing.T) {
	faulted := &StoreFaults{AppendFault: NewFault("store.Append", Always())}
	if _, err := faulted.Append("session", nil); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("append error = %v, want the injected failure", err)
	}
	if _, err := faulted.Append("session", nil); err == nil {
		t.Fatal("the schedule did not stay in effect")
	}
	if faulted.AppendFault.Calls() != 2 || faulted.AppendFault.Failures() != 2 {
		t.Fatalf("calls = %d failures = %d", faulted.AppendFault.Calls(), faulted.AppendFault.Failures())
	}

	unwired := &StoreFaults{}
	if _, err := unwired.Append("session", nil); err == nil || !strings.Contains(err.Error(), "no delegate") {
		t.Fatalf("append error = %v, want a missing delegate", err)
	}
	if err := unwired.Save(session.Snapshot{}); err == nil || !strings.Contains(err.Error(), "no delegate") {
		t.Fatalf("save error = %v, want a missing delegate", err)
	}
}
