package tool

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"Eylu/internal/policy"
)

// shellSyntaxSentinel is the token a probe's second command prints. A line that
// starts with it is a line the shell split, because the only command that prints it
// alone is the one behind the separator.
const shellSyntaxSentinel = "EYLU_SHELL_SENTINEL_7F3A"

// quotedSyntaxProbes are the constructs the classifier and a shell can disagree
// about. Every line only echoes, so running one cannot change anything.
var quotedSyntaxProbes = []struct {
	name string
	line string
}{
	{name: "an ampersand inside single quotes", line: `echo left 'x & echo ` + shellSyntaxSentinel + `' right`},
	{name: "a pipe inside single quotes", line: `echo left 'x | echo ` + shellSyntaxSentinel + `' right`},
	{name: "a redirect inside single quotes", line: `echo left 'x > ` + shellSyntaxSentinel + `' right`},
	{name: "a backtick inside single quotes", line: "echo left 'x `echo " + shellSyntaxSentinel + "`' right"},
	{name: "an ampersand inside double quotes", line: `echo left "x & echo ` + shellSyntaxSentinel + `" right`},
	{name: "a pipe inside double quotes", line: `echo left "x | echo ` + shellSyntaxSentinel + `" right`},
	{name: "a redirect inside double quotes", line: `echo left "x > ` + shellSyntaxSentinel + `" right`},
}

// probeShells returns the shells a probe can be run against on this platform.
//
// The Windows command interpreter is only reachable where it exists, and a POSIX
// shell is skipped when the platform has none. A candidate that cannot be probed is
// reported as skipped rather than as verified.
func probeShells() []ShellAdapter {
	shells := make([]ShellAdapter, 0, 2)
	if runtime.GOOS == "windows" {
		if comspec := os.Getenv("COMSPEC"); comspec != "" {
			shells = append(shells, commandShell{name: "cmd", path: comspec, args: []string{"/d", "/s", "/c"}})
		}
		for _, candidate := range []string{
			filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "bash.exe"),
			filepath.Join(os.Getenv("ProgramFiles"), "Git", "usr", "bin", "bash.exe"),
		} {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				shells = append(shells, commandShell{name: "git-bash", path: candidate, args: []string{"-lc"}})
				break
			}
		}
		return shells
	}
	if _, err := os.Stat("/bin/sh"); err == nil {
		shells = append(shells, commandShell{name: "sh", path: "/bin/sh", args: []string{"-lc"}})
	}
	return shells
}

// shellSyntaxObservation is what one shell actually did with a probe line.
type shellSyntaxObservation struct {
	// secondCommand reports that a command behind the construct ran.
	secondCommand bool
	// createdFiles are the files the shell created while running the line, which is
	// how a redirection the classifier did not see shows up.
	createdFiles []string
	// output is kept for the failure message.
	output string
}

func (o shellSyntaxObservation) active() bool {
	return o.secondCommand || len(o.createdFiles) > 0
}

// observeShellSyntax runs one probe line through one shell inside its own directory
// and reports what the shell did with it.
func observeShellSyntax(t *testing.T, shell ShellAdapter, line string) shellSyntaxObservation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	directory := t.TempDir()
	command := shell.Command(ctx, line)
	command.Dir = directory
	command.Env = minimalEnvironment()
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	_ = command.Run()

	observation := shellSyntaxObservation{output: output.String()}
	for _, value := range strings.Split(strings.ReplaceAll(output.String(), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(value), shellSyntaxSentinel) {
			observation.secondCommand = true
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		observation.createdFiles = append(observation.createdFiles, entry.Name())
	}
	return observation
}

// positiveControlProbes carry the same constructs without any quoting. No shell
// treats these as inert text, so they are what proves the comparison can see an
// active construct at all: without them a run that observed nothing would look like
// a run that agreed.
var positiveControlProbes = []struct {
	name string
	line string
}{
	{name: "an unquoted ampersand", line: `echo left & echo ` + shellSyntaxSentinel + ` right`},
	{name: "an unquoted pipe", line: `echo left | echo ` + shellSyntaxSentinel + ` right`},
	{name: "an unquoted redirect", line: `echo left > ` + shellSyntaxSentinel + ` right`},
}

// The classifier reads a command line the way the shell that will run it does.
//
// The direction that matters is one-way: a line the classifier reads as inert text
// must be inert to the shell. Where the classifier is stricter than the shell
// nothing is lost, because a stricter reading only asks for confirmation; where it
// is looser, a separator or a redirection it did not see is a second command or a
// written file that was never classified.
//
// The comparison is non-destructive: every probe line only echoes, and the shell
// runs inside a temporary directory whose contents are inspected afterwards.
func TestCommandClassificationAgreesWithTheShellThatRunsIt(t *testing.T) {
	for _, shell := range probeShells() {
		t.Run(shell.Name(), func(t *testing.T) {
			config := policy.DefaultConfig(policy.ModeManual)
			if shell.Name() == "cmd" {
				config.Shell = policy.ShellCommandPrompt
			}
			// The positive control first: the harness has to be able to see an
			// active construct before its silence about a quoted one means anything.
			for _, probe := range positiveControlProbes {
				observed := observeShellSyntax(t, shell, probe.line)
				if !observed.active() {
					t.Fatalf("%s did not treat the unquoted control %q as active, so this comparison cannot detect anything: %+v",
						shell.Name(), probe.line, observed)
				}
			}
			for _, probe := range quotedSyntaxProbes {
				t.Run(probe.name, func(t *testing.T) {
					observed := observeShellSyntax(t, shell, probe.line)
					if policy.ReadsInertText(probe.line, config) && observed.active() {
						t.Fatalf("the classifier read the line as inert text while %s found active syntax: %+v", shell.Name(), observed)
					}
				})
			}
		})
	}
}

// A line the classifier cannot prove inert must not be classified read-only: the
// confirmation is what the operator relies on when the reading is uncertain.
func TestAConstructTheCommandInterpreterReadsDifferentlyIsNotReadOnly(t *testing.T) {
	cmd := policy.DefaultConfig(policy.ModeManual)
	cmd.Shell = policy.ShellCommandPrompt
	for _, line := range []string{
		// The interpreter has no single-quote grouping, so the separator is active.
		`find . -name '*.go & echo marker'`,
		`git status 'x | echo marker'`,
		`git log -1 'x > marker'`,
		// Expansion and the interpreter's own escape change the argument.
		`git status --short %PATH%`,
		`git status ^& echo marker`,
	} {
		if class := policy.ClassifyCommand(line, cmd); class == policy.CommandReadOnly {
			t.Fatalf("%q was classified read-only under the command interpreter", line)
		}
	}
	// Double quotes do group in the interpreter, so a construct inside them keeps
	// its meaning and the line stays readable.
	if class := policy.ClassifyCommand(`git log -1 "x > marker"`, cmd); class == policy.CommandUnknown {
		t.Fatalf("a double-quoted operand was refused under the command interpreter: %s", class)
	}
	// Under a POSIX shell the single-quoted forms really are inert, so the same
	// lines keep the classification they always had.
	posix := policy.DefaultConfig(policy.ModeManual)
	for _, line := range []string{`find . -name '*.go & echo marker'`, `git status 'x | echo marker'`} {
		if class := policy.ClassifyCommand(line, posix); class == policy.CommandUnknown {
			t.Fatalf("%q was refused under a POSIX shell: %s", line, class)
		}
	}
}
