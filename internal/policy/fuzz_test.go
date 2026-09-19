package policy

import (
	"strings"
	"testing"
)

// The classifier is the component that decides whether a command may run without
// a human looking at it, and its input space is every command line a model can
// write. Finite examples cannot cover that: what follows states the properties
// that must hold for every line, and lets the fuzzing engine look for the input
// that breaks one.
const (
	fuzzDangerousMarker = "zz-danger-marker"
	fuzzBlockedMarker   = "zz-blocked-marker"
)

// fuzzCommands seeds the classifier with the shapes that matter: a separator the
// shell acts on, a separator inside quotes, a construct only one dialect treats as
// active, and the boundary forms of the reader itself.
var fuzzCommands = []string{
	"ls -la",
	"git status --porcelain",
	"git diff --cached --stat",
	"go test ./...",
	"cat file.txt",
	"echo 'a & echo b'",
	`echo "a | echo b"`,
	"echo \"a & echo b\"",
	"ls; ls",
	"ls | cat",
	"ls && rm -rf /",
	"rm -rf /",
	"find . -name '*.go' -delete",
	"git branch -D main",
	"cat file > other",
	"echo `id`",
	"echo $(id)",
	"echo $((1+1))",
	"echo %PATH%",
	"echo ^&",
	"dir & del *",
	"sed -i 's/a/b/' file",
	"git diff --output=out.patch",
	"rg --pre=cat pattern",
	"--",
	"-",
	"'",
	`"`,
	"\\",
	"echo \"unterminated",
	"echo \\'",
	"\n;\n|\n&\n",
	strings.Repeat("a", 4096),
	"A_COMMAND " + strings.Repeat("--option ", 200),
	"ünïcödé --fläg",
}

// FuzzClassifyCommand states what must be true of every command line.
//
// The properties are one-directional: they say what a read-only verdict may not
// be granted to, which is the direction a mistake would be dangerous in.
func FuzzClassifyCommand(f *testing.F) {
	for _, seed := range fuzzCommands {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, command string) {
		for _, dialect := range []ShellDialect{ShellPOSIX, ShellCommandPrompt} {
			config := DefaultConfig(ModeAuto)
			config.Shell = dialect
			config.DangerousPatterns = append(append([]string(nil), config.DangerousPatterns...), fuzzDangerousMarker)
			config.BlockedPatterns = append(append([]string(nil), config.BlockedPatterns...), fuzzBlockedMarker)

			class := ClassifyCommand(command, config)
			if !validCommandClass(class) {
				t.Fatalf("classify %q (%s) = %q, which is not a command class", command, dialect, class)
			}
			// The answer may not depend on anything but the input.
			if again := ClassifyCommand(command, config); again != class {
				t.Fatalf("classify %q (%s) is not deterministic: %q then %q", command, dialect, class, again)
			}
			// Reading the line is a total function too, on both dialects.
			hasActiveShellSyntax(command, dialect)
			segments := Segments(command, config)
			for _, segment := range segments {
				if strings.TrimSpace(segment) == "" {
					t.Fatalf("split %q (%s) produced an empty segment: %#v", command, dialect, segments)
				}
			}
			if class != CommandReadOnly {
				continue
			}

			// 1. A line carrying a pattern the configuration calls dangerous or
			//    blocked is never read-only, whatever order the reader saw it in.
			lowered := strings.ToLower(strings.TrimSpace(command))
			if strings.Contains(lowered, fuzzDangerousMarker) || strings.Contains(lowered, fuzzBlockedMarker) {
				t.Fatalf("classify %q (%s) = read-only with a dangerous or blocked pattern in the line", command, dialect)
			}
			// 2. A line with active shell syntax is never read-only: the reader
			//    could not prove what it does, so it may not be granted anything.
			if hasActiveShellSyntax(strings.TrimSpace(command), dialect) {
				t.Fatalf("classify %q (%s) = read-only with active shell syntax", command, dialect)
			}
			// 3. The whole is only read-only when every part is. This is the
			//    property that makes a separator safe: a read-only line whose
			//    second command writes would be a write that was never classified.
			for _, segment := range segments {
				if part := ClassifyCommand(segment, config); part != CommandReadOnly {
					t.Fatalf("classify %q (%s) = read-only, but its segment %q is %q", command, dialect, segment, part)
				}
			}
		}
	})
}

// fuzzArguments seeds the argument validator with the forms the rules exist for.
var fuzzArguments = []string{
	"", "-", "--", "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fls", "-fprint", "-fprintf",
	"--pre=cat", "--pre", "--output=out", "--ext-diff", "--textconv", "--show-signature",
	"--open-files-in-pager=less", "-O", "--help", "-d", "-D", "--delete", "-m", "-M", "--move",
	"-c", "-C", "--copy", "-f", "--force", "-t", "--track", "-u", "--set-upstream-to", "--unset-upstream",
	"-u", "--porcelain=v2", "-vv", "-la", "main", "HEAD~1", "--stat", "--cached", "-z", "-b", "--short",
	"--", "..", "*", "ünïcödé", strings.Repeat("x", 512),
}

// FuzzReadOnlyArgumentVerdict states that validating one command invocation is
// total and deterministic for every command that has a rule.
//
// The verdict decides whether a command is read-only, auto-allowed or refused, so
// a value outside the three would silently become one of them through the default
// branch of a switch.
func FuzzReadOnlyArgumentVerdict(f *testing.F) {
	for _, seed := range fuzzArguments {
		f.Add(seed, seed)
	}
	f.Fuzz(func(t *testing.T, first, second string) {
		args := []string{first, second}
		for name, rule := range readOnlyArgumentRules {
			verdict := rule(args)
			if !validArgumentVerdict(verdict) {
				t.Fatalf("%s(%q, %q) = %d, which is not a verdict", name, first, second, verdict)
			}
			if again := rule(args); again != verdict {
				t.Fatalf("%s(%q, %q) is not deterministic: %d then %d", name, first, second, verdict, again)
			}
			// The rule may not read the slice it was handed: a mutation would
			// change the answer for whoever holds the same arguments.
			if len(args) != 2 || args[0] != first || args[1] != second {
				t.Fatalf("%s rewrote its arguments: %#v", name, args)
			}
		}
	})
}

// TestDeclaredDangerousArgumentFamiliesAreNeverReadOnly pins the option families
// the classifier treats as unprovable, so removing one from a rule is a failure
// rather than a quiet widening of the read-only set.
//
// The list is deliberately written out again here instead of read from the rules:
// a test that reads the same table it is checking only proves the table equals
// itself, while this one fails when a family is removed from the table.
func TestDeclaredDangerousArgumentFamiliesAreNeverReadOnly(t *testing.T) {
	families := []struct {
		command string
		forms   []string
	}{
		{"find", []string{"-delete", "-exec", "-execdir", "-ok", "-okdir", "-fls", "-fprint", "-fprint0", "-fprintf"}},
		{"rg", []string{"--pre", "--pre=cat", "--preamble"}},
		{"git diff", []string{"--ext-diff", "--textconv", "--output=out.patch", "--show-signature", "--open-files-in-pager=less", "-O", "--help"}},
		{"git log", []string{"--ext-diff", "--textconv", "--output=out.patch", "--show-signature", "--open-files-in-pager=less", "-O", "--help"}},
		{"git show", []string{"--ext-diff", "--textconv", "--output=out.patch", "--show-signature", "--open-files-in-pager=less", "-O", "--help"}},
		{"git rev-parse", []string{"--ext-diff", "--textconv", "--show-signature", "--help"}},
		{"git ls-files", []string{"--ext-diff", "--textconv", "--help"}},
		{"git grep", []string{"--ext-diff", "--textconv", "--show-signature", "--help"}},
		{"git branch", []string{
			"-d", "-D", "--delete", "-m", "-M", "--move", "-c", "-C", "--copy", "-f", "--force",
			"-t", "--track", "-u", "--set-upstream-to", "--unset-upstream", "--edit-description",
		}},
		// `git branch <name>` creates a branch, and a bare operand is only a
		// listing pattern next to one of the listing options.
		{"git branch", []string{}},
	}
	for _, family := range families {
		if _, known := readOnlyArgumentRules[family.command]; !known {
			t.Fatalf("%s has no argument rule, so this test is checking nothing", family.command)
		}
		for _, form := range family.forms {
			if verdict := validateReadOnlyArguments(family.command, []string{form}); verdict == verdictReadOnly {
				t.Fatalf("%s %s was classified read-only", family.command, form)
			}
		}
	}
	if verdict := validateReadOnlyArguments("git branch", []string{"new-branch"}); verdict == verdictReadOnly {
		t.Fatal("a bare operand was read as a listing rather than as branch creation")
	}
	// A command with no rule is read-only only in its bare form.
	if verdict := validateReadOnlyArguments("unconfigured-command", []string{"--anything"}); verdict == verdictReadOnly {
		t.Fatal("an argument was accepted for a command with no rules")
	}
	if verdict := validateReadOnlyArguments("unconfigured-command", nil); verdict != verdictReadOnly {
		t.Fatal("the bare form of a configured command was refused")
	}
}

func validCommandClass(class CommandClass) bool {
	switch class {
	case CommandReadOnly, CommandAutoAllowed, CommandUnknown, CommandDangerous, CommandBlocked:
		return true
	default:
		return false
	}
}

func validArgumentVerdict(verdict argumentVerdict) bool {
	switch verdict {
	case verdictReadOnly, verdictUnknown, verdictDangerous:
		return true
	default:
		return false
	}
}
