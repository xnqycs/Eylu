package policy

import (
	"context"
	"encoding/json"
	"testing"
)

func TestPermissionModeRiskMatrix(t *testing.T) {
	tests := []struct {
		name          string
		mode          PermissionMode
		request       Request
		decision      Decision
		confirmations int
		class         CommandClass
	}{
		{name: "manual read", mode: ModeManual, request: Request{Tool: "read_file", Risk: RiskRead}, decision: DecisionAllow, class: CommandNotApplicable},
		{name: "manual session", mode: ModeManual, request: Request{Tool: "todolist", Risk: RiskSession}, decision: DecisionAllow, class: CommandNotApplicable},
		{name: "manual write", mode: ModeManual, request: Request{Tool: "edit_file", Risk: RiskWrite}, decision: DecisionConfirm, confirmations: 1, class: CommandNotApplicable},
		{name: "manual dangerous", mode: ModeManual, request: bashRequest("git reset --hard HEAD"), decision: DecisionConfirm, confirmations: 2, class: CommandDangerous},
		{name: "plan write", mode: ModePlan, request: Request{Tool: "write_file", Risk: RiskWrite}, decision: DecisionDeny, class: CommandNotApplicable},
		{name: "plan read command", mode: ModePlan, request: bashRequest("git status --short && git diff"), decision: DecisionAllow, class: CommandReadOnly},
		{name: "plan build", mode: ModePlan, request: bashRequest("go build ./..."), decision: DecisionDeny, class: CommandAutoAllowed},
		{name: "auto write", mode: ModeAuto, request: Request{Tool: "edit_file", Risk: RiskWrite}, decision: DecisionAllow, class: CommandNotApplicable},
		{name: "auto build", mode: ModeAuto, request: bashRequest("go test ./..."), decision: DecisionAllow, class: CommandAutoAllowed},
		{name: "auto unknown", mode: ModeAuto, request: bashRequest("npm install"), decision: DecisionConfirm, confirmations: 1, class: CommandUnknown},
		{name: "auto dangerous", mode: ModeAuto, request: bashRequest("rm -rf build"), decision: DecisionConfirm, confirmations: 2, class: CommandDangerous},
		{name: "full unknown", mode: ModeFull, request: bashRequest("npm install"), decision: DecisionAllow, class: CommandUnknown},
		{name: "full dangerous", mode: ModeFull, request: bashRequest("git push --force origin main"), decision: DecisionConfirm, confirmations: 1, class: CommandDangerous},
		{name: "full high", mode: ModeFull, request: Request{Tool: "danger", Risk: RiskHigh}, decision: DecisionConfirm, confirmations: 1, class: CommandNotApplicable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outcome := NewChecker(DefaultConfig(test.mode)).Check(context.Background(), test.request)
			if outcome.Decision != test.decision || outcome.Confirmations != test.confirmations || outcome.Classification != test.class || outcome.Mode != test.mode {
				t.Fatalf("outcome = %#v", outcome)
			}
			if test.class == CommandDangerous && !outcome.Warning && outcome.Decision == DecisionConfirm {
				t.Fatal("dangerous confirmation lacks a warning")
			}
		})
	}
}

func TestCommandClassificationAndBlockedPatterns(t *testing.T) {
	config := DefaultConfig(ModeAuto)
	config.BlockedPatterns = []string{"shutdown"}
	config.AutoAllowCommands = append(config.AutoAllowCommands, "pnpm test")
	tests := map[string]CommandClass{
		`git status && git diff --stat`: CommandReadOnly,
		`go test ./...`:                 CommandAutoAllowed,
		`pnpm test --runInBand`:         CommandAutoAllowed,
		`echo "a;b"`:                    CommandUnknown,
		`go test ./... | tee out.log`:   CommandUnknown,
		`git diff > patch.txt`:          CommandUnknown,
		`git status $(echo bad)`:        CommandUnknown,
		"git status `echo bad`":         CommandUnknown,
		`shutdown /s`:                   CommandBlocked,
		`Remove-Item -Recurse build`:    CommandDangerous,
	}
	for command, expected := range tests {
		if got := ClassifyCommand(command, config); got != expected {
			t.Fatalf("ClassifyCommand(%q) = %s, want %s", command, got, expected)
		}
	}
	outcome := NewChecker(config).Check(context.Background(), bashRequest("shutdown /s"))
	if outcome.Decision != DecisionDeny {
		t.Fatalf("blocked outcome = %#v", outcome)
	}
}

func TestParseMode(t *testing.T) {
	for _, value := range []string{"manual", "plan", "auto", "full"} {
		mode, err := ParseMode(value)
		if err != nil || mode.String() != value {
			t.Fatalf("ParseMode(%q) = %s, %v", value, mode, err)
		}
	}
	if _, err := ParseMode("unsafe"); err == nil {
		t.Fatal("expected invalid mode error")
	}
}

func bashRequest(command string) Request {
	input, _ := json.Marshal(map[string]string{"command": command})
	return Request{Tool: "bash", Risk: RiskExec, Input: input}
}
