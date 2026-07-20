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
		shell = defaultShell()
	}
	if maxOutputBytes <= 0 {
		maxOutputBytes = 64 << 10
	}
	return &Bash{paths: paths, workspace: paths.real, shell: shell, maxOutputBytes: maxOutputBytes}, nil
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
		return ConcurrencySpec{Mode: ConcurrencyShared}
	}
	return ConcurrencySpec{Mode: ConcurrencyExclusive}
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

func defaultShell() ShellAdapter {
	if configured := os.Getenv("EYLU_SHELL"); configured != "" {
		return commandShell{name: filepath.Base(configured), path: configured, args: []string{"-lc"}}
	}
	if runtime.GOOS == "windows" {
		candidates := []string{
			filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "bash.exe"),
			filepath.Join(os.Getenv("ProgramFiles"), "Git", "usr", "bin", "bash.exe"),
		}
		for _, candidate := range candidates {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return commandShell{name: "git-bash", path: candidate, args: []string{"-lc"}}
			}
		}
		return commandShell{name: "cmd", path: os.Getenv("COMSPEC"), args: []string{"/d", "/s", "/c"}}
	}
	return commandShell{name: "sh", path: "/bin/sh", args: []string{"-lc"}}
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
