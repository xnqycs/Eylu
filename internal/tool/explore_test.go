package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchCodeLiteralRegexGlobAndLimits(t *testing.T) {
	workspace := t.TempDir()
	writeTestFile(t, workspace, "main.go", "package main\nfunc Alpha() {}\n")
	writeTestFile(t, workspace, "nested/util.go", "package nested\nfunc AlphaBeta() {}\n")
	writeTestFile(t, workspace, "nested/readme.md", "Alpha docs\n")
	writeTestFile(t, workspace, ".hidden.go", "func AlphaHidden() {}\n")
	if err := os.WriteFile(filepath.Join(workspace, "binary.bin"), []byte{'A', 0, 'B'}, 0o600); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, workspace, "large.txt", strings.Repeat("Alpha", 100))
	index, _ := newRepositoryIndex(workspace, func(context.Context, string, ...string) ([]byte, error) { return nil, os.ErrNotExist })
	search := NewSearchCode(index, 10, 100)
	result := search.Execute(context.Background(), json.RawMessage(`{"query":"Alpha","glob":"**/*.go"}`))
	if result.IsError {
		t.Fatalf("result = %#v", result)
	}
	var payload struct {
		Matches       []SearchMatch `json:"matches"`
		SkippedBinary int           `json:"skipped_binary"`
		SkippedLarge  int           `json:"skipped_large"`
	}
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Matches) != 3 || payload.Matches[0].Path != ".hidden.go" || payload.Matches[1].Path != "main.go" || payload.Matches[2].Path != "nested/util.go" {
		t.Fatalf("matches = %#v", payload.Matches)
	}
	result = search.Execute(context.Background(), json.RawMessage(`{"query":"Alpha(Beta)?","regex":true,"glob":"nested/*","max_results":1}`))
	if result.IsError || !result.Truncated || !strings.Contains(result.Content, "nested/readme.md") {
		t.Fatalf("regex result = %#v", result)
	}
	result = search.Execute(context.Background(), json.RawMessage(`{"query":"[","regex":true}`))
	if !result.IsError || !strings.Contains(result.Content, "invalid regular expression") {
		t.Fatalf("invalid regex result = %#v", result)
	}
	result = search.Execute(context.Background(), json.RawMessage(`{"query":"Alpha"}`))
	if !strings.Contains(result.Content, `"skipped_binary": 1`) || !strings.Contains(result.Content, `"skipped_large": 1`) {
		t.Fatalf("skip counters = %s", result.Content)
	}
}

func TestListDirectoryDepthHiddenAndTraversal(t *testing.T) {
	workspace := t.TempDir()
	writeTestFile(t, workspace, "main.go", "package main")
	writeTestFile(t, workspace, "nested/deep/util.go", "package deep")
	writeTestFile(t, workspace, ".hidden/secret.txt", "secret")
	index, _ := newRepositoryIndex(workspace, func(context.Context, string, ...string) ([]byte, error) { return nil, os.ErrNotExist })
	list := NewListDirectory(index, 100)
	result := list.Execute(context.Background(), json.RawMessage(`{"depth":0}`))
	if result.IsError || strings.Contains(result.Content, "secret.txt") || strings.Contains(result.Content, "util.go") || !strings.Contains(result.Content, "nested/") {
		t.Fatalf("depth result = %#v", result)
	}
	result = list.Execute(context.Background(), json.RawMessage(`{"depth":5,"include_hidden":true}`))
	if result.IsError || !strings.Contains(result.Content, "secret.txt") || !strings.Contains(result.Content, "util.go") {
		t.Fatalf("hidden result = %#v", result)
	}
	result = list.Execute(context.Background(), json.RawMessage(`{"path":"nested","depth":1,"max_entries":1}`))
	if result.IsError || !result.Truncated || !strings.Contains(result.Content, "truncated") {
		t.Fatalf("truncated result = %#v", result)
	}
	result = list.Execute(context.Background(), json.RawMessage(`{"path":"../outside"}`))
	if !result.IsError || !strings.Contains(result.Content, "outside workspace") {
		t.Fatalf("traversal result = %#v", result)
	}
}
