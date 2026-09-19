package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type ShellAdapter interface {
	Name() string
	Command(context.Context, string) *exec.Cmd
}

type commandShell struct {
	name string
	path string
	args []string
}

func (s commandShell) Name() string { return s.name }
func (s commandShell) Command(ctx context.Context, command string) *exec.Cmd {
	args := append(append([]string(nil), s.args...), command)
	return exec.CommandContext(ctx, s.path, args...)
}

type Bash struct {
	paths          *pathResolver
	workspace      string
	shell          ShellAdapter
	maxOutputBytes int
	environment    []string
	context        *CodeContext
}

func (b *Bash) AllowEnvironment(names []string) {
	b.environment = append([]string(nil), names...)
}

func NewBash(workspace string, maxOutputBytes int, shell ShellAdapter) (*Bash, error) {
	paths, err := newPathResolver(workspace)
	if err != nil {
		return nil, err
	}
	if shell == nil {
		resolved, err := defaultShell()
		if err != nil {
			return nil, err
		}
		shell = resolved
	}
	if maxOutputBytes <= 0 {
		maxOutputBytes = 64 << 10
	}
	return &Bash{paths: paths, workspace: paths.real, shell: shell, maxOutputBytes: maxOutputBytes}, nil
}

func NewBashWithContext(codeContext *CodeContext, maxOutputBytes int, shell ShellAdapter) (*Bash, error) {
	bash, err := NewBash(codeContext.index.workspace, maxOutputBytes, shell)
	if err != nil {
		return nil, err
	}
	bash.context = codeContext
	return bash, nil
}

func (b *Bash) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{
		Name:        "bash",
		Description: "Run a shell command inside the workspace to build, test, format, or diagnose the repository. The command has a timeout, a minimal inherited environment, separate stdout/stderr capture, exit-code reporting, and bounded output. The active platform shell is reported in the result.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"Command executed by the platform shell"},"working_directory":{"type":"string","description":"Optional workspace-relative directory"},"reason":{"type":"string","minLength":1,"description":"User-facing reason"}},"required":["command","reason"],"additionalProperties":false}`),
	}
}

func (b *Bash) Risk() policy.Risk { return policy.RiskExec }

func (b *Bash) ClassifyConcurrency(_ json.RawMessage, outcome policy.Outcome) ConcurrencySpec {
	if outcome.Classification == policy.CommandReadOnly {
		// The tree claim uses the same canonical key as the file tools, so a
		// read-only command and a write to the same tree are always detected as
		// conflicting on every platform.
		return ConcurrencySpec{Mode: ConcurrencyClaimed, Claims: []ResourceClaim{{Kind: ResourceTree, Path: b.paths.rootResourceKey(), Access: ResourceRead}}}
	}
	return ConcurrencySpec{Mode: ConcurrencyExclusive}
}

func (b *Bash) AfterExecute(outcome policy.Outcome) {
	if b.context != nil && outcome.Classification != policy.CommandReadOnly {
		b.context.InvalidateAll()
	}
}

func (b *Bash) Execute(ctx context.Context, raw json.RawMessage) protocol.ToolResult {
	var input struct {
		Command          string `json:"command"`
		WorkingDirectory string `json:"working_directory"`
		Reason           string `json:"reason"`
	}
	if err := decodeStrict(raw, &input); err != nil {
		return toolError("invalid bash input: " + err.Error())
	}
	if strings.TrimSpace(input.Command) == "" {
		return toolError("command is required")
	}
	workingDirectory := b.workspace
	if input.WorkingDirectory != "" {
		resolved, err := b.paths.existing(input.WorkingDirectory)
		if err != nil {
			return toolError("resolve working directory: " + err.Error())
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			return toolError("working_directory is not a directory")
		}
		workingDirectory = resolved
	}
	command := b.shell.Command(ctx, input.Command)
	command.Dir = workingDirectory
	command.Env = minimalEnvironment(b.environment...)
	stdout := &cappedBuffer{limit: b.maxOutputBytes}
	stderr := &cappedBuffer{limit: b.maxOutputBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	err := runCommandTree(ctx, command)
	exitCode := 0
	if err != nil {
		if ctx.Err() != nil {
			return toolError("command cancelled: " + ctx.Err().Error())
		}
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode = exitError.ExitCode()
		} else {
			return toolError("start command: " + err.Error())
		}
	}
	content := fmt.Sprintf("shell: %s\nexit_code: %d\nstdout:\n%s\nstderr:\n%s", b.shell.Name(), exitCode, stdout.String(), stderr.String())
	return protocol.ToolResult{
		Content: content, IsError: exitCode != 0, Truncated: stdout.truncated || stderr.truncated,
		Metadata: map[string]any{"shell": b.shell.Name(), "exit_code": exitCode, "stdout_bytes": stdout.total, "stderr_bytes": stderr.total},
	}
}

type cappedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	total     int
	truncated bool
}

func (b *cappedBuffer) Write(value []byte) (int, error) {
	b.total += len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if remaining > len(value) {
			remaining = len(value)
		}
		_, _ = b.buffer.Write(value[:remaining])
	}
	if b.total > b.limit {
		b.truncated = true
	}
	return len(value), nil
}

func (b *cappedBuffer) String() string { return strings.ToValidUTF8(b.buffer.String(), "�") }

// ActiveShellDialect reports how the shell this process would run commands with
// reads a command line, so the policy can read it the same way.
//
// It exists because the two readings have to agree: a construct the policy believes
// is quoted text but the shell treats as a separator is a second command that was
// never classified. The tool knows which shell it would use, so it answers rather
// than letting the policy guess.
//
// A shell whose rules are not modelled is reported as an error instead of being
// answered with the closest dialect. Reading a PowerShell command line with POSIX
// rules is worse than not supporting PowerShell: the policy would report a
// classification it cannot justify, and the user would believe it.
func ActiveShellDialect() (policy.ShellDialect, error) {
	shell, err := defaultShell()
	if err != nil {
		return "", err
	}
	if isCommandInterpreter(shell.Name()) {
		return policy.ShellCommandPrompt, nil
	}
	return policy.ShellPOSIX, nil
}

// UnsupportedShellError reports a configured shell whose command line rules Eylu
// does not model.
//
// It is an error and not a fallback because the fallback is the dangerous state:
// the classifier would read the line with POSIX quoting and separator rules while
// the shell reads it with its own, and a command the policy called read-only could
// run something else entirely.
type UnsupportedShellError struct {
	// Path is the executable EYLU_SHELL named.
	Path string
}

func (e *UnsupportedShellError) Error() string {
	return "EYLU_SHELL points at " + e.Path + ", whose command line rules Eylu does not model: " +
		"reading its commands with POSIX rules could classify as read-only a line the shell reads as a separator, " +
		"which is the one thing the classifier must never do. Unset EYLU_SHELL, or point it at a POSIX shell or the command interpreter"
}

// isUnmodelledShell reports whether an executable name is a shell whose command
// line rules the policy layer does not implement. Only PowerShell is named: a
// shell Eylu has never heard of is not assumed to be PowerShell, and refusing
// every unknown name would make EYLU_SHELL useless.
func isUnmodelledShell(name string) bool {
	switch shellName(name) {
	case "powershell", "pwsh":
		return true
	default:
		return false
	}
}

// isCommandInterpreter reports whether an executable name is the Windows command
// interpreter, whose dialect is the other one the policy models.
func isCommandInterpreter(name string) bool {
	return shellName(name) == "cmd"
}

// shellName reduces an executable name to the form the dialect table is written
// in: lower case, without its directory and without its extension.
//
// Both separators are recognised whatever the host is. A dialect is a property of
// the shell, not of the machine reading the configuration, so `filepath.Base` is
// deliberately not used here: on Linux it would take `C:\...\pwsh.exe` as one file
// name, decide the shell is not PowerShell, and return the one answer that must
// never be given. The extension is stripped the same way, for the same reason.
func shellName(name string) string {
	trimmed := strings.TrimSpace(name)
	if index := strings.LastIndexAny(trimmed, `/\`); index >= 0 {
		trimmed = trimmed[index+1:]
	}
	base := strings.ToLower(trimmed)
	// A leading dot names a hidden file rather than an extension, so only a dot
	// with something in front of it is stripped.
	if index := strings.LastIndexByte(base, '.'); index > 0 {
		base = base[:index]
	}
	return base
}

func defaultShell() (ShellAdapter, error) {
	if configured := strings.TrimSpace(os.Getenv("EYLU_SHELL")); configured != "" {
		if isUnmodelledShell(configured) {
			return nil, &UnsupportedShellError{Path: configured}
		}
		// The command interpreter takes its own flags; handing it the POSIX `-lc`
		// would not run the command at all, and naming it without its extension is
		// what keeps the dialect lookup honest.
		if isCommandInterpreter(configured) {
			return commandShell{name: "cmd", path: configured, args: []string{"/d", "/s", "/c"}}, nil
		}
		return commandShell{name: filepath.Base(configured), path: configured, args: []string{"-lc"}}, nil
	}
	if runtime.GOOS == "windows" {
		candidates := []string{
			filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "bash.exe"),
			filepath.Join(os.Getenv("ProgramFiles"), "Git", "usr", "bin", "bash.exe"),
		}
		for _, candidate := range candidates {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return commandShell{name: "git-bash", path: candidate, args: []string{"-lc"}}, nil
			}
		}
		return commandShell{name: "cmd", path: os.Getenv("COMSPEC"), args: []string{"/d", "/s", "/c"}}, nil
	}
	return commandShell{name: "sh", path: "/bin/sh", args: []string{"-lc"}}, nil
}

// ValidateShell reports whether the shell this process would run commands with is
// one whose rules the policy layer models.
//
// It exists so that a configuration can be refused at the point it is applied,
// with a message about the setting, rather than at the point a command is
// classified, with a message about the command.
func ValidateShell() error {
	_, err := defaultShell()
	return err
}

func minimalEnvironment(extra ...string) []string {
	allowed := map[string]struct{}{
		"PATH": {}, "HOME": {}, "USERPROFILE": {}, "TEMP": {}, "TMP": {}, "SYSTEMROOT": {}, "COMSPEC": {}, "PATHEXT": {},
		"LOCALAPPDATA": {}, "APPDATA": {}, "XDG_CACHE_HOME": {}, "LANG": {}, "LC_ALL": {}, "TERM": {}, "NO_COLOR": {},
		"GOPATH": {}, "GOROOT": {}, "GOCACHE": {}, "GOENV": {}, "GOMODCACHE": {},
	}
	for _, name := range extra {
		name = strings.ToUpper(strings.TrimSpace(name))
		if name != "" {
			allowed[name] = struct{}{}
		}
	}
	values := make(map[string]string)
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if _, keep := allowed[strings.ToUpper(key)]; keep {
			values[key] = value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}
