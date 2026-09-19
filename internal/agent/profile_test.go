package agent

import (
	"testing"

	"Eylu/internal/policy"
)

// T-04: a subagent's authority is derived from its parent's permission mode and
// can only be narrower.
//
// The policy checker is what actually decides whether a call may run, and a
// subagent shares its parent's checker. What the profile must never do is hand
// the child a wider mode than the request that delegated to it, because every
// promise made to the user about `plan`, `manual`, `auto` or `full` is a promise
// about that mode.
func TestSubagentPermissionCannotBeWiderThanTheParent(t *testing.T) {
	for _, mode := range []string{"manual", "plan", "auto", "full"} {
		parent := ProfileForMode(mode)
		child := GeneralSubagentProfile(mode, 5)
		if child.PermissionMode != parent.PermissionMode {
			t.Fatalf("mode %q: the subagent runs as %q while its parent runs as %q", mode, child.PermissionMode, parent.PermissionMode)
		}
		if child.PermissionMode != mode {
			t.Fatalf("mode %q: the subagent runs as %q", mode, child.PermissionMode)
		}
		if !child.Isolated {
			t.Fatalf("mode %q: the subagent shares the parent's context instead of a derived one", mode)
		}
	}
	// An unset mode narrows to the strictest named mode in both profiles rather
	// than widening to whatever the caller happened to leave blank.
	if parent, child := ProfileForMode(""), GeneralSubagentProfile("", 5); parent.PermissionMode != "manual" || child.PermissionMode != "manual" {
		t.Fatalf("an unset mode gave parent %q and child %q", parent.PermissionMode, child.PermissionMode)
	}
}

// T-05: a subagent cannot delegate again and cannot interrogate the user.
//
// Recursion has no bound of its own here, and a question asked on behalf of a
// delegated task reaches a user who never saw the task; both are refused at the
// profile, so the child's registry never holds the tools at all.
func TestSubagentCannotSpawnAnotherSubagentOrAskTheUser(t *testing.T) {
	profile := GeneralSubagentProfile("full", 5)
	for _, name := range []string{"agent", "task_output", "task_stop", "ask", "activate_skill"} {
		if profile.AllowsTool(name, policy.RiskRead) || profile.AllowsTool(name, policy.RiskSession) {
			t.Fatalf("a subagent profile allows %q", name)
		}
	}
	// The refusal is a subtraction, not a replacement: the ordinary tools a
	// delegated task needs are still there.
	for _, allowed := range []struct {
		name string
		risk policy.Risk
	}{{"read_file", policy.RiskRead}, {"edit_file", policy.RiskWrite}, {"bash", policy.RiskExec}} {
		if !profile.AllowsTool(allowed.name, allowed.risk) {
			t.Fatalf("a subagent profile lost %q", allowed.name)
		}
	}
}
