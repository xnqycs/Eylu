package tool

import (
	"errors"
	"strings"
	"testing"

	"Eylu/internal/policy"
)

// S-07 / the classifier's promise: a shell whose command line rules Eylu does not
// model is refused, not guessed at.
//
// PowerShell's backtick escapes, single-quote semantics and statement separators
// are not POSIX's. Until this was refused, pointing EYLU_SHELL at it made the
// policy read a PowerShell command line with POSIX rules and report a
// classification it could not justify - which is worse than not supporting
// PowerShell, because the user believes the classifier is protecting them.
func TestAnUnmodelledShellIsRefusedRatherThanReadAsPOSIX(t *testing.T) {
	for _, configured := range []string{
		"powershell.exe", "PowerShell.EXE", "powershell", "pwsh", "pwsh.exe", "PWSH.EXE",
		`C:\Program Files\PowerShell\7\pwsh.exe`, "/usr/bin/pwsh", "/opt/microsoft/powershell/powershell",
	} {
		t.Run(configured, func(t *testing.T) {
			t.Setenv("EYLU_SHELL", configured)
			err := ValidateShell()
			if err == nil {
				t.Fatal("an unmodelled shell was accepted")
			}
			var refused *UnsupportedShellError
			if !errors.As(err, &refused) {
				t.Fatalf("error = %v (%T), want an UnsupportedShellError", err, err)
			}
			if refused.Path != configured {
				t.Fatalf("the error names %q, want the configured %q", refused.Path, configured)
			}
			// The message has to say what to do, because this is a configuration
			// mistake rather than a crash.
			for _, expected := range []string{"EYLU_SHELL", "POSIX", "Unset"} {
				if !strings.Contains(err.Error(), expected) {
					t.Fatalf("the message does not mention %q: %s", expected, err)
				}
			}
			if _, dialectErr := ActiveShellDialect(); dialectErr == nil {
				t.Fatal("the dialect was answered for an unmodelled shell")
			}
			if _, bashErr := NewBash(t.TempDir(), 1024, nil); bashErr == nil {
				t.Fatal("the bash tool was built around an unmodelled shell")
			}
		})
	}
}

// The default resolution is unchanged: no EYLU_SHELL, the command interpreter, or
// a POSIX shell all resolve to the dialect they always did.
func TestTheModelledShellsAreUnaffected(t *testing.T) {
	t.Setenv("EYLU_SHELL", "")
	dialect, err := ActiveShellDialect()
	if err != nil {
		t.Fatalf("the platform default was refused: %v", err)
	}
	if dialect != policy.ShellPOSIX && dialect != policy.ShellCommandPrompt {
		t.Fatalf("platform default dialect = %q", dialect)
	}
	if err := ValidateShell(); err != nil {
		t.Fatalf("the platform default was refused: %v", err)
	}

	for _, configured := range []string{"/bin/bash", "/bin/sh", "bash", "sh", "zsh", `C:\Program Files\Git\bin\bash.exe`, "cmd.exe"} {
		t.Run(configured, func(t *testing.T) {
			t.Setenv("EYLU_SHELL", configured)
			if err := ValidateShell(); err != nil {
				t.Fatalf("a modelled shell was refused: %v", err)
			}
			dialect, err := ActiveShellDialect()
			if err != nil {
				t.Fatal(err)
			}
			want := policy.ShellPOSIX
			if isUnmodelledShell("cmd") || strings.HasPrefix(strings.ToLower(configured), "cmd") {
				want = policy.ShellCommandPrompt
			}
			if dialect != want {
				t.Fatalf("dialect = %q, want %q", dialect, want)
			}
		})
	}
}

func TestOnlyPowerShellNamesAreUnmodelled(t *testing.T) {
	for _, name := range []string{"powershell.exe", "PowerShell.EXE", "pwsh", "pwsh.exe", "/usr/bin/pwsh"} {
		if !isUnmodelledShell(name) {
			t.Fatalf("%q was treated as modelled", name)
		}
	}
	// A name is compared without its extension and without its case, but nothing
	// else is guessed at: a shell Eylu has never heard of is not assumed to be
	// PowerShell.
	for _, name := range []string{"bash", "bash.exe", "/bin/sh", "cmd.exe", "zsh", "fish", "powershell_ise", "my-pwsh-wrapper", ""} {
		if isUnmodelledShell(name) {
			t.Fatalf("%q was treated as PowerShell", name)
		}
	}
}
