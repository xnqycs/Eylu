package policy

import "testing"

// A tool-level decision may never relax an explicit operator prohibition.
func TestComposeCannotRelaxHardDeny(t *testing.T) {
	config := DefaultConfig(ModeFull)
	config.BlockedPatterns = []string{"curl"}
	base := NewChecker(config).Check(t.Context(), bashRequest(`curl https://example.com`))
	if !base.HardDeny {
		t.Fatalf("base = %#v", base)
	}
	override := Outcome{Decision: DecisionAllow, Source: SourceTool, Rule: "tool_domain"}
	composed := Compose(base, &override)
	if composed.Decision != DecisionDeny || !composed.HardDeny {
		t.Fatalf("composed decision = %#v", composed)
	}
	if composed.Source != SourceWorkspace || composed.Rule != "blocked_pattern" {
		t.Fatalf("composed source = %s rule = %s", composed.Source, composed.Rule)
	}
	if composed.Override != "tool:tool_domain" {
		t.Fatalf("override record = %q", composed.Override)
	}
}

// An unrecognized or zero decision must never authorize execution.
func TestComposeFailsClosedOnUnrecognizedDecision(t *testing.T) {
	if composed := Compose(Outcome{}, nil); composed.Decision != DecisionDeny || composed.Rule != "unrecognized_decision" {
		t.Fatalf("zero outcome = %#v", composed)
	}
	base := Outcome{Decision: DecisionAllow, Mode: ModeAuto, Risk: RiskExec, Source: SourceWorkspace, Rule: "auto:allowlist"}
	override := Outcome{Decision: Decision("maybe")}
	if composed := Compose(base, &override); composed.Decision != DecisionDeny || composed.Rule != "unrecognized_decision" {
		t.Fatalf("unknown override = %#v", composed)
	}
}

// The mode always comes from the workspace layer, so audit never misreports the
// active mode after a tool-level override.
func TestComposeKeepsWorkspaceModeAndRisk(t *testing.T) {
	base := Outcome{Decision: DecisionDeny, Mode: ModePlan, Risk: RiskNetwork, Classification: CommandNotApplicable, Source: SourceWorkspace, Rule: "plan:default_deny"}
	override := Outcome{Decision: DecisionAllow, Source: SourceTool, Rule: "web_permission:allow"}
	composed := Compose(base, &override)
	if composed.Decision != DecisionAllow || composed.Mode != ModePlan || composed.Risk != RiskNetwork {
		t.Fatalf("composed = %#v", composed)
	}
	if composed.Source != SourceTool || composed.Rule != "web_permission:allow" {
		t.Fatalf("composed source = %s rule = %s", composed.Source, composed.Rule)
	}
	if composed.Override != "" {
		t.Fatalf("applied override recorded as ignored: %q", composed.Override)
	}
}

func TestComposeWithoutOverrideNormalizesBase(t *testing.T) {
	base := Outcome{Decision: DecisionRestrictedForTest, Mode: ModeManual}
	composed := Compose(base, nil)
	if composed.Decision != DecisionDeny || !composed.HardDeny || composed.Source != SourceWorkspace {
		t.Fatalf("composed = %#v", composed)
	}
}

// DecisionRestrictedForTest is an out-of-band decision value that a custom
// checker could return by mistake.
const DecisionRestrictedForTest Decision = "restricted"
