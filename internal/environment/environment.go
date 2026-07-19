package environment

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	commandTimeout        = 2 * time.Second
	maxStatusBytes        = 16 << 10
	statusTruncatedMarker = "\n[git status truncated]\n"
)

type Context struct {
	WorkingDirectory string `json:"working_directory,omitempty"`
	IsGitRepo        bool   `json:"is_git_repo,omitempty"`
	Platform         string `json:"platform,omitempty"`
	OSVersion        string `json:"os_version,omitempty"`
	Today            string `json:"today,omitempty"`
	CurrentBranch    string `json:"current_branch,omitempty"`
	MainBranch       string `json:"main_branch,omitempty"`
	Status           string `json:"status,omitempty"`
	RecentCommits    string `json:"recent_commits,omitempty"`
}

type CommandRunner func(ctx context.Context, directory, name string, args ...string) (string, error)

type Collector struct {
	Platform string
	Now      func() time.Time
	ReadFile func(string) ([]byte, error)
	Run      CommandRunner
}

func Capture(ctx context.Context, workspace string) Context {
	return (Collector{}).Capture(ctx, workspace)
}

func (c Collector) Capture(ctx context.Context, workspace string) Context {
	platform := c.Platform
	if platform == "" {
		platform = runtime.GOOS
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}
	readFile := c.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	run := c.Run
	if run == nil {
		run = runCommand
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		absolute = workspace
	}
	snapshot := Context{
		WorkingDirectory: filepath.Clean(absolute),
		Platform:         platform,
		Today:            now().Format("2006-01-02"),
	}
	snapshot.OSVersion = detectOSVersion(ctx, platform, readFile, run)
	if snapshot.OSVersion == "" {
		snapshot.OSVersion = platform + "/" + runtime.GOARCH
	}

	inside, err := runWithTimeout(ctx, run, snapshot.WorkingDirectory, "git", "rev-parse", "--is-inside-work-tree")
	if err != nil || !strings.EqualFold(strings.TrimSpace(inside), "true") {
		return snapshot
	}
	snapshot.IsGitRepo = true
	namedBranch := ""
	if branch, branchErr := runWithTimeout(ctx, run, snapshot.WorkingDirectory, "git", "symbolic-ref", "--quiet", "--short", "HEAD"); branchErr == nil && strings.TrimSpace(branch) != "" {
		namedBranch = strings.TrimSpace(branch)
		snapshot.CurrentBranch = namedBranch
	} else if revision, revisionErr := runWithTimeout(ctx, run, snapshot.WorkingDirectory, "git", "rev-parse", "--short", "HEAD"); revisionErr == nil && strings.TrimSpace(revision) != "" {
		snapshot.CurrentBranch = "HEAD detached at " + strings.TrimSpace(revision)
	} else {
		snapshot.CurrentBranch = "(unknown)"
	}
	snapshot.MainBranch = detectMainBranch(ctx, run, snapshot.WorkingDirectory, namedBranch)

	status, statusErr := runWithTimeout(ctx, run, snapshot.WorkingDirectory, "git", "status", "--short")
	if statusErr != nil {
		snapshot.Status = "(unknown)"
	} else if status = strings.Trim(status, "\r\n"); status == "" {
		snapshot.Status = "(clean)"
	} else {
		snapshot.Status = truncateStatus(status)
	}
	recent, recentErr := runWithTimeout(ctx, run, snapshot.WorkingDirectory, "git", "log", "-5", "--pretty=format:%h %s")
	if recentErr != nil || strings.TrimSpace(recent) == "" {
		snapshot.RecentCommits = "(none)"
	} else {
		snapshot.RecentCommits = strings.TrimSpace(recent)
	}
	return snapshot
}

func (c Context) Empty() bool {
	return strings.TrimSpace(c.WorkingDirectory) == ""
}

func (c Context) IsZero() bool {
	return c.Empty()
}

func (c Context) Prompt(modelID string) string {
	if c.Empty() {
		return ""
	}
	var prompt strings.Builder
	prompt.WriteString("Here is useful information about the environment you are running in:\n<env>\n")
	fmt.Fprintf(&prompt, "Working directory: %s\n", sanitizeScalar(c.WorkingDirectory))
	if c.IsGitRepo {
		prompt.WriteString("Is directory a git repo: Yes\n")
	} else {
		prompt.WriteString("Is directory a git repo: No\n")
	}
	fmt.Fprintf(&prompt, "Platform: %s\n", sanitizeScalar(c.Platform))
	fmt.Fprintf(&prompt, "OS Version: %s\n", sanitizeScalar(c.OSVersion))
	fmt.Fprintf(&prompt, "Today's date: %s\n", sanitizeScalar(c.Today))
	prompt.WriteString("</env>")
	if strings.TrimSpace(modelID) != "" {
		fmt.Fprintf(&prompt, "\nYour model ID is %s.", sanitizeScalar(modelID))
	}
	if !c.IsGitRepo {
		return prompt.String()
	}
	prompt.WriteString("\ngitStatus: This is the git status at the start of the conversation. Note that this status is a snapshot in time, and will not update during the conversation.\n")
	fmt.Fprintf(&prompt, "Current branch: %s\n\n", sanitizeScalar(c.CurrentBranch))
	fmt.Fprintf(&prompt, "Main branch (you will usually use this for PRs): %s\n\n", sanitizeScalar(c.MainBranch))
	prompt.WriteString("Status:\n")
	prompt.WriteString(sanitizeMultiline(c.Status))
	prompt.WriteString("\n\nRecent commits:\n")
	prompt.WriteString(sanitizeMultiline(c.RecentCommits))
	return prompt.String()
}

func detectMainBranch(ctx context.Context, run CommandRunner, workspace, current string) string {
	remote, err := runWithTimeout(ctx, run, workspace, "git", "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if err == nil {
		remote = strings.TrimSpace(remote)
		if remote != "" {
			return strings.TrimPrefix(remote, "origin/")
		}
	}
	for _, branch := range []string{"main", "master"} {
		if _, branchErr := runWithTimeout(ctx, run, workspace, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch); branchErr == nil {
			return branch
		}
	}
	if current != "" {
		return current
	}
	return "(unknown)"
}

func detectOSVersion(ctx context.Context, platform string, readFile func(string) ([]byte, error), run CommandRunner) string {
	switch platform {
	case "windows":
		output, _ := runWithTimeout(ctx, run, "", "cmd", "/c", "ver")
		return strings.TrimSpace(output)
	case "linux":
		if data, err := readFile("/etc/os-release"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if !strings.HasPrefix(line, "PRETTY_NAME=") {
					continue
				}
				value := strings.TrimPrefix(line, "PRETTY_NAME=")
				if unquoted, err := strconv.Unquote(value); err == nil {
					return strings.TrimSpace(unquoted)
				}
				return strings.Trim(strings.TrimSpace(value), "'\"")
			}
		}
	case "darwin":
		if version, err := runWithTimeout(ctx, run, "", "sw_vers", "-productVersion"); err == nil && strings.TrimSpace(version) != "" {
			return "macOS " + strings.TrimSpace(version)
		}
	}
	output, _ := runWithTimeout(ctx, run, "", "uname", "-sr")
	return strings.TrimSpace(output)
}

func runWithTimeout(ctx context.Context, run CommandRunner, directory, name string, args ...string) (string, error) {
	commandContext, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	return run(commandContext, directory, name, args...)
}

func runCommand(ctx context.Context, directory, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	if directory != "" {
		command.Dir = directory
	}
	output, err := command.Output()
	return string(output), err
}

func truncateStatus(status string) string {
	if len([]byte(status)) <= maxStatusBytes {
		return status
	}
	available := maxStatusBytes - len(statusTruncatedMarker)
	headBytes := available * 2 / 3
	tailBytes := available - headBytes
	head := utf8Prefix(status, headBytes)
	tail := utf8Suffix(status, tailBytes)
	return head + statusTruncatedMarker + tail
}

func utf8Prefix(value string, limit int) string {
	prefix := value[:min(len(value), limit)]
	for len(prefix) > 0 && !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix
}

func utf8Suffix(value string, limit int) string {
	start := max(0, len(value)-limit)
	for start < len(value) && !utf8.ValidString(value[start:]) {
		start++
	}
	return value[start:]
}

func sanitizeScalar(value string) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	return escapeTags(strings.TrimSpace(value))
}

func sanitizeMultiline(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	return escapeTags(strings.Trim(value, "\n"))
}

func escapeTags(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return replacer.Replace(value)
}
