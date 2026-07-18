package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type helperShell struct{}

func (helperShell) Name() string { return "test-shell" }
func (helperShell) Command(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, os.Args[0], "-test.run=TestBashHelperProcess", "--", command)
}

func TestBashHelperProcess(t *testing.T) {
	separator := -1
	for index, arg := range os.Args {
		if arg == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		return
	}
	switch os.Args[separator+1] {
	case "success":
		fmt.Fprint(os.Stdout, "stdout-value")
		fmt.Fprint(os.Stderr, "stderr-value")
	case "failure":
		fmt.Fprint(os.Stderr, "failed")
		os.Exit(7)
	case "large":
		fmt.Fprint(os.Stdout, strings.Repeat("x", 200))
	case "sleep":
		time.Sleep(time.Second)
	default:
		if strings.HasPrefix(os.Args[separator+1], "tree|") {
			marker := strings.TrimPrefix(os.Args[separator+1], "tree|")
			time.Sleep(50 * time.Millisecond)
			child := exec.Command(os.Args[0], "-test.run=TestBashTreeChildProcess", "--", marker)
			_ = child.Start()
			time.Sleep(time.Second)
		}
	}
	os.Exit(0)
}

func TestBashTreeChildProcess(t *testing.T) {
	separator := -1
	for index, arg := range os.Args {
		if arg == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		return
	}
	time.Sleep(400 * time.Millisecond)
	_ = os.WriteFile(os.Args[separator+1], []byte("survived"), 0o600)
	os.Exit(0)
}

func TestBashResultTimeoutAndTruncation(t *testing.T) {
	bashTool, err := NewBash(t.TempDir(), 32, helperShell{})
	if err != nil {
		t.Fatal(err)
	}
	result := bashTool.Execute(context.Background(), json.RawMessage(`{"command":"success"}`))
	if result.IsError || !strings.Contains(result.Content, "exit_code: 0") || !strings.Contains(result.Content, "stdout-value") || !strings.Contains(result.Content, "stderr-value") {
		t.Fatalf("success = %#v", result)
	}
	result = bashTool.Execute(context.Background(), json.RawMessage(`{"command":"failure"}`))
	if !result.IsError || result.Metadata["exit_code"] != 7 {
		t.Fatalf("failure = %#v", result)
	}
	result = bashTool.Execute(context.Background(), json.RawMessage(`{"command":"large"}`))
	if result.IsError || !result.Truncated || result.Metadata["stdout_bytes"] != 200 {
		t.Fatalf("large = %#v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	result = bashTool.Execute(ctx, json.RawMessage(`{"command":"sleep"}`))
	if !result.IsError || !strings.Contains(result.Content, "cancelled") {
		t.Fatalf("cancel = %#v", result)
	}
}

func TestBashCancellationKillsProcessTree(t *testing.T) {
	workspace := t.TempDir()
	marker := filepath.Join(workspace, "child-survived")
	bashTool, err := NewBash(workspace, 1024, helperShell{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	result := bashTool.Execute(ctx, mustJSON(t, map[string]any{"command": "tree|" + marker}))
	if !result.IsError || !strings.Contains(result.Content, "cancelled") {
		t.Fatalf("result = %#v", result)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("child process survived cancellation: %v", err)
	}
}
