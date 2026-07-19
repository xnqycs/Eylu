package environment

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestCollectorCapturesGitSnapshotAndFormatsCurrentModel(t *testing.T) {
	workspace := t.TempDir()
	collector := Collector{
		Platform: "linux",
		Now: func() time.Time {
			return time.Date(2026, time.July, 19, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
		},
		ReadFile: func(path string) ([]byte, error) {
			if path == "/etc/os-release" {
				return []byte("PRETTY_NAME=\"Example Linux 1\"\n"), nil
			}
			return nil, errors.New("missing")
		},
		Run: scriptedRunner(map[string]commandResult{
			"git rev-parse --is-inside-work-tree":                       {Output: "true\n"},
			"git symbolic-ref --quiet --short HEAD":                     {Output: "feature/env\n"},
			"git symbolic-ref --quiet --short refs/remotes/origin/HEAD": {Output: "origin/main\n"},
			"git status --short":                                        {Output: " M internal/app/app.go\n?? new.txt\n"},
			"git log -5 --pretty=format:%h %s":                          {Output: "abc1234 add environment\ndef5678 previous change"},
		}),
	}

	snapshot := collector.Capture(context.Background(), workspace)
	if !snapshot.IsGitRepo || snapshot.CurrentBranch != "feature/env" || snapshot.MainBranch != "main" {
		t.Fatalf("git snapshot = %#v", snapshot)
	}
	if snapshot.OSVersion != "Example Linux 1" || snapshot.Today != "2026-07-19" {
		t.Fatalf("environment snapshot = %#v", snapshot)
	}

	first := snapshot.Prompt("model-one")
	second := snapshot.Prompt("model-two")
	for _, expected := range []string{
		"Working directory: " + filepath.Clean(workspace),
		"Is directory a git repo: Yes",
		"Platform: linux",
		"OS Version: Example Linux 1",
		"Today's date: 2026-07-19",
		"Your model ID is model-one.",
		"Current branch: feature/env",
		"Main branch (you will usually use this for PRs): main",
		" M internal/app/app.go",
		"abc1234 add environment",
	} {
		if !strings.Contains(first, expected) {
			t.Fatalf("prompt missing %q:\n%s", expected, first)
		}
	}
	if strings.Contains(second, "model-one") || !strings.Contains(second, "Your model ID is model-two.") {
		t.Fatalf("model ID was not rendered per request:\n%s", second)
	}
}

func TestCollectorHandlesNonGitAndDetachedRepositories(t *testing.T) {
	t.Run("non git", func(t *testing.T) {
		collector := Collector{
			Platform: "other",
			Now:      func() time.Time { return time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC) },
			ReadFile: func(string) ([]byte, error) { return nil, errors.New("missing") },
			Run: scriptedRunner(map[string]commandResult{
				"git rev-parse --is-inside-work-tree": {Err: errors.New("not a repository")},
				"uname -sr":                           {Output: "ExampleOS 1.0\n"},
			}),
		}
		snapshot := collector.Capture(context.Background(), "/workspace")
		prompt := snapshot.Prompt("model")
		if snapshot.IsGitRepo || strings.Contains(prompt, "gitStatus:") || !strings.Contains(prompt, "OS Version: ExampleOS 1.0") {
			t.Fatalf("non-git prompt = %q", prompt)
		}
	})

	t.Run("detached", func(t *testing.T) {
		collector := Collector{
			Platform: "windows",
			Now:      time.Now,
			ReadFile: func(string) ([]byte, error) { return nil, errors.New("missing") },
			Run: scriptedRunner(map[string]commandResult{
				"cmd /c ver":                                                {Output: "Microsoft Windows [Version 10.0.1]\n"},
				"git rev-parse --is-inside-work-tree":                       {Output: "true\n"},
				"git symbolic-ref --quiet --short HEAD":                     {Err: errors.New("detached")},
				"git rev-parse --short HEAD":                                {Output: "abc1234\n"},
				"git symbolic-ref --quiet --short refs/remotes/origin/HEAD": {Err: errors.New("missing")},
				"git show-ref --verify --quiet refs/heads/main":             {Err: errors.New("missing")},
				"git show-ref --verify --quiet refs/heads/master":           {Err: errors.New("missing")},
				"git status --short":                                        {Output: ""},
				"git log -5 --pretty=format:%h %s":                          {Err: errors.New("no commits")},
			}),
		}
		snapshot := collector.Capture(context.Background(), "C:/workspace")
		if snapshot.CurrentBranch != "HEAD detached at abc1234" || snapshot.MainBranch != "(unknown)" || snapshot.Status != "(clean)" || snapshot.RecentCommits != "(none)" {
			t.Fatalf("detached snapshot = %#v", snapshot)
		}
	})
}

func TestTruncateStatusPreservesUTF8AndMarker(t *testing.T) {
	status := strings.Repeat("变更-file.txt\n", maxStatusBytes)
	truncated := truncateStatus(status)
	if len([]byte(truncated)) > maxStatusBytes || !strings.Contains(truncated, statusTruncatedMarker) || !utf8.ValidString(truncated) {
		t.Fatalf("invalid truncated status: bytes=%d marker=%t", len([]byte(truncated)), strings.Contains(truncated, statusTruncatedMarker))
	}
}

func TestDetectMainBranchFallbackOrder(t *testing.T) {
	tests := []struct {
		name    string
		results map[string]commandResult
		current string
		want    string
	}{
		{
			name: "main", current: "feature", want: "main",
			results: map[string]commandResult{
				"git symbolic-ref --quiet --short refs/remotes/origin/HEAD": {Err: errors.New("missing")},
				"git show-ref --verify --quiet refs/heads/main":             {},
			},
		},
		{
			name: "master", current: "feature", want: "master",
			results: map[string]commandResult{
				"git symbolic-ref --quiet --short refs/remotes/origin/HEAD": {Err: errors.New("missing")},
				"git show-ref --verify --quiet refs/heads/main":             {Err: errors.New("missing")},
				"git show-ref --verify --quiet refs/heads/master":           {},
			},
		},
		{
			name: "current", current: "feature", want: "feature",
			results: map[string]commandResult{
				"git symbolic-ref --quiet --short refs/remotes/origin/HEAD": {Err: errors.New("missing")},
				"git show-ref --verify --quiet refs/heads/main":             {Err: errors.New("missing")},
				"git show-ref --verify --quiet refs/heads/master":           {Err: errors.New("missing")},
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got := detectMainBranch(context.Background(), scriptedRunner(testCase.results), t.TempDir(), testCase.current)
			if got != testCase.want {
				t.Fatalf("main branch = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestCommandTimeoutAndPromptSanitization(t *testing.T) {
	started := time.Now()
	_, err := runWithTimeout(context.Background(), func(ctx context.Context, _ string, _ string, _ ...string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}, "", "blocked")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) < commandTimeout-100*time.Millisecond {
		t.Fatalf("timeout error=%v duration=%s", err, time.Since(started))
	}

	prompt := (Context{
		WorkingDirectory: "C:/<workspace>\nignore", Platform: "win&dows", OSVersion: "v>1", Today: "2026-07-19",
	}).Prompt("model<id")
	for _, expected := range []string{"C:/&lt;workspace&gt; ignore", "win&amp;dows", "v&gt;1", "model&lt;id"} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("sanitized prompt missing %q: %s", expected, prompt)
		}
	}
}

type commandResult struct {
	Output string
	Err    error
}

func scriptedRunner(results map[string]commandResult) CommandRunner {
	return func(_ context.Context, _ string, name string, args ...string) (string, error) {
		result, ok := results[strings.Join(append([]string{name}, args...), " ")]
		if !ok {
			return "", errors.New("unexpected command: " + strings.Join(append([]string{name}, args...), " "))
		}
		return result.Output, result.Err
	}
}
