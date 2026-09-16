package policy

import "testing"

// A configured read-only prefix must never make every argument form read-only.
func TestReadOnlyClassificationRejectsSideEffectArguments(t *testing.T) {
	config := DefaultConfig(ModePlan)
	tests := map[string]CommandClass{
		// Query forms stay read-only.
		`find . -name '*.go'`:                CommandReadOnly,
		`find . -maxdepth 2 -type f -name a`: CommandReadOnly,
		`git branch`:                         CommandReadOnly,
		`git branch -a`:                      CommandReadOnly,
		`git branch -vv`:                     CommandReadOnly,
		`git branch --list 'feat/*'`:         CommandReadOnly,
		`git branch --contains HEAD`:         CommandReadOnly,
		`git branch -a feature`:              CommandReadOnly,
		`git status --short`:                 CommandReadOnly,
		`git status --porcelain=v2 -z`:       CommandReadOnly,
		`git log --oneline -5`:               CommandReadOnly,
		`git diff --stat`:                    CommandReadOnly,
		`git diff --no-ext-diff`:             CommandReadOnly,
		`git rev-parse HEAD`:                 CommandReadOnly,
		`git ls-files --stage`:               CommandReadOnly,
		`ls -la src`:                         CommandReadOnly,
		`pwd`:                                CommandReadOnly,
		`pwd -P`:                             CommandReadOnly,
		`rg -n 'pattern' internal`:           CommandReadOnly,

		// Argument forms that delete, execute, or write.
		`find . -delete`:                         CommandDangerous,
		`find . -exec rm {} ;`:                   CommandDangerous,
		`find . -execdir rm {} ;`:                CommandDangerous,
		`find . -ok rm {} ;`:                     CommandDangerous,
		`find . -fprint out.txt`:                 CommandDangerous,
		`find . -fprintf out.txt %p`:             CommandDangerous,
		`find . -fls out.txt`:                    CommandDangerous,
		`rg --pre 'cat' pattern`:                 CommandDangerous,
		`git branch -D feature`:                  CommandDangerous,
		`git branch -d feature`:                  CommandDangerous,
		`git branch -m old new`:                  CommandDangerous,
		`git branch -M old new`:                  CommandDangerous,
		`git branch -c old new`:                  CommandDangerous,
		`git branch --delete feature`:            CommandDangerous,
		`git branch --set-upstream-to=origin`:    CommandDangerous,
		`git branch --edit-description`:          CommandDangerous,
		`git branch -f feature`:                  CommandDangerous,
		`git diff --ext-diff`:                    CommandDangerous,
		`git diff --textconv`:                    CommandDangerous,
		`git diff --output=patch.txt`:            CommandDangerous,
		`git log --show-signature`:               CommandDangerous,
		`git grep -O pager pattern`:              CommandDangerous,
		`git grep --open-files-in-pager pattern`: CommandDangerous,
		`git -c core.pager=evil log`:             CommandDangerous,
		`git --config-env=core.pager=EVIL log`:   CommandDangerous,
		`git --exec-path=/tmp log`:               CommandDangerous,
		`git -p log`:                             CommandDangerous,

		// Forms that cannot be proven safe fall back to unknown.
		`git branch new-branch`:       CommandUnknown,
		`git -C /tmp log`:             CommandUnknown,
		`git --git-dir=/tmp/repo log`: CommandUnknown,
		`git branch --unknown-query`:  CommandUnknown,
		`pwd extra`:                   CommandUnknown,
		`git log --help`:              CommandUnknown,
	}
	for command, expected := range tests {
		if got := ClassifyCommand(command, config); got != expected {
			t.Errorf("ClassifyCommand(%q) = %s, want %s", command, got, expected)
		}
	}
}

// Shell syntax and expansion change the argument after classification, so they
// are never read-only.
func TestUnprovableShellSyntaxFallsBackToUnknown(t *testing.T) {
	config := DefaultConfig(ModePlan)
	tests := map[string]CommandClass{
		`git status && git log --oneline`:   CommandReadOnly,
		`git status | cat`:                  CommandUnknown,
		`git status > out.txt`:              CommandUnknown,
		`git status < in.txt`:               CommandUnknown,
		"git status `echo bad`":             CommandUnknown,
		`git status $(echo bad)`:            CommandUnknown,
		`git log --grep=$PATTERN`:           CommandUnknown,
		`git log --grep=${PATTERN}`:         CommandUnknown,
		`find . -name "$UNQUOTED_PATTERN"`:  CommandUnknown,
		`git status 'unclosed`:              CommandUnknown,
		"git status \\":                     CommandUnknown,
		`git status --short; git branch -a`: CommandReadOnly,
		`git status; git branch -D feature`: CommandDangerous,
	}
	for command, expected := range tests {
		if got := ClassifyCommand(command, config); got != expected {
			t.Errorf("ClassifyCommand(%q) = %s, want %s", command, got, expected)
		}
	}
}

// A configured command without argument rules is only read-only in its bare
// form, so an operator cannot widen the policy with a prefix entry.
func TestConfiguredCommandWithoutRulesRequiresBareForm(t *testing.T) {
	config := DefaultConfig(ModePlan)
	config.ReadOnlyCommands = append(config.ReadOnlyCommands, "docker ps")
	if got := ClassifyCommand(`docker ps`, config); got != CommandReadOnly {
		t.Fatalf("docker ps = %s, want read_only", got)
	}
	if got := ClassifyCommand(`docker ps -a`, config); got != CommandUnknown {
		t.Fatalf("docker ps -a = %s, want unknown", got)
	}
	// A two-token prefix must not match an unrelated invocation.
	if got := ClassifyCommand(`git statusx`, config); got != CommandUnknown {
		t.Fatalf("git statusx = %s, want unknown", got)
	}
}

// The mode matrix must stay conservative for known side-effecting arguments.
func TestSideEffectingArgumentsAcrossModes(t *testing.T) {
	tests := []struct {
		mode          PermissionMode
		decision      Decision
		confirmations int
		warning       bool
		rule          string
	}{
		{mode: ModePlan, decision: DecisionDeny, rule: "plan:default_deny"},
		{mode: ModeManual, decision: DecisionConfirm, confirmations: 2, warning: true, rule: "manual:dangerous_operation"},
		{mode: ModeAuto, decision: DecisionConfirm, confirmations: 2, warning: true, rule: "auto:dangerous_command"},
		{mode: ModeFull, decision: DecisionConfirm, confirmations: 1, warning: true, rule: "full:dangerous_operation"},
	}
	for _, test := range tests {
		outcome := NewChecker(DefaultConfig(test.mode)).Check(t.Context(), bashRequest(`find . -delete`))
		if outcome.Decision != test.decision || outcome.Confirmations != test.confirmations || outcome.Warning != test.warning || outcome.Rule != test.rule {
			t.Errorf("mode %s outcome = %#v", test.mode, outcome)
		}
		if outcome.Classification != CommandDangerous || outcome.Risk != RiskHigh {
			t.Errorf("mode %s classification = %s risk = %s", test.mode, outcome.Classification, outcome.Risk)
		}
		if outcome.Source != SourceWorkspace {
			t.Errorf("mode %s source = %s", test.mode, outcome.Source)
		}
	}
}

func TestReadOnlyOutcomeIsAuditableAndNotHardDenied(t *testing.T) {
	outcome := NewChecker(DefaultConfig(ModePlan)).Check(t.Context(), bashRequest(`git status --short`))
	if outcome.Decision != DecisionAllow || outcome.Rule != "plan:read_only_command" || outcome.HardDeny {
		t.Fatalf("outcome = %#v", outcome)
	}
}

// Only an explicit operator prohibition is non-relaxable.
func TestBlockedPatternIsHardDenied(t *testing.T) {
	config := DefaultConfig(ModeFull)
	config.BlockedPatterns = []string{"curl"}
	outcome := NewChecker(config).Check(t.Context(), bashRequest(`curl https://example.com`))
	if outcome.Decision != DecisionDeny || !outcome.HardDeny || outcome.Rule != "blocked_pattern" {
		t.Fatalf("outcome = %#v", outcome)
	}
}

// Dangerous and blocked patterns are matched as trimmed substrings of the whole
// lowercased command line. The default `format ` entry therefore also matches an
// unrelated `--format=` argument, which keeps the classification conservative.
// This test pins the current behaviour so a future change to it is deliberate.
func TestDangerousPatternsMatchTrimmedSubstrings(t *testing.T) {
	config := DefaultConfig(ModePlan)
	if got := ClassifyCommand(`git log --format=%H`, config); got != CommandDangerous {
		t.Fatalf("git log --format=%%H = %s, want dangerous (trimmed substring pattern)", got)
	}
	config.DangerousPatterns = nil
	if got := ClassifyCommand(`git log --format=%H`, config); got != CommandReadOnly {
		t.Fatalf("git log --format=%%H without the pattern = %s, want read_only", got)
	}
}
