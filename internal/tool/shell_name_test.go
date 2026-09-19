package tool

import "testing"

// The name a dialect is looked up by does not depend on the host.
//
// This is the bug the three-platform matrix found: filepath.Base on Linux takes
// `C:\Program Files\PowerShell\7\pwsh.exe` as a single file name, so the shell was
// not recognised and the silent POSIX fallback that S-07 exists to remove came back
// on two of the three platforms. A shell is a property of the target, not of the
// machine reading the configuration, so both separators are recognised everywhere.
func TestTheShellNameDoesNotDependOnTheHost(t *testing.T) {
	for _, testCase := range []struct{ name, want string }{
		{"pwsh", "pwsh"},
		{"pwsh.exe", "pwsh"},
		{"PWSH.EXE", "pwsh"},
		{"/usr/bin/pwsh", "pwsh"},
		{`C:\Program Files\PowerShell\7\pwsh.exe`, "pwsh"},
		{`C:/Program Files/PowerShell/7/pwsh.exe`, "pwsh"},
		{"/opt/microsoft/powershell/powershell", "powershell"},
		{`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, "powershell"},
		{"  /bin/bash  ", "bash"},
		{"/bin/sh", "sh"},
		{"cmd.exe", "cmd"},
		{`C:\Windows\System32\cmd.exe`, "cmd"},
		{"powershell_ise", "powershell_ise"},
		{"my-pwsh-wrapper", "my-pwsh-wrapper"},
		{"", ""},
		{" ", ""},
		{".hidden", ".hidden"},
		{"trailing.", "trailing"},
		{`\`, ""},
		{`C:\`, ""},
	} {
		if got := shellName(testCase.name); got != testCase.want {
			t.Fatalf("shellName(%q) = %q, want %q", testCase.name, got, testCase.want)
		}
	}
	// The same decision follows from the name however the path around it was
	// written, which is the property the failure was about.
	for _, name := range []string{
		`C:\Program Files\PowerShell\7\pwsh.exe`,
		"/opt/microsoft/powershell/powershell",
		"PowerShell.EXE",
		`C:\...\pwsh`,
		`D:\tools\pwsh`,
	} {
		if !isUnmodelledShell(name) {
			t.Fatalf("%q was not recognised as PowerShell", name)
		}
	}
	for _, name := range []string{`C:\Windows\System32\cmd.exe`, "/cmd", "cmd", "CMD.EXE", `\\server\share\cmd.exe`} {
		if !isCommandInterpreter(name) {
			t.Fatalf("%q was not recognised as the command interpreter", name)
		}
	}
	// A name that is neither is neither, whatever path it arrived in.
	for _, name := range []string{"/bin/bash", `C:\Program Files\Git\bin\bash.exe`, "zsh", "fish", ""} {
		if isUnmodelledShell(name) || isCommandInterpreter(name) {
			t.Fatalf("%q was recognised as a shell the policy models differently", name)
		}
	}
}
