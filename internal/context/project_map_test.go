package context

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildProjectMapStableInventory(t *testing.T) {
	workspace := t.TempDir()
	writeMapFile(t, filepath.Join(workspace, "main.go"), "package main")
	writeMapFile(t, filepath.Join(workspace, "go.mod"), "module demo")
	writeMapFile(t, filepath.Join(workspace, "web", "index.ts"), "export {}")
	writeMapFile(t, filepath.Join(workspace, "node_modules", "hidden.js"), "hidden")
	first, err := BuildProjectMap(workspace, DefaultProjectMapBytes)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildProjectMap(workspace, DefaultProjectMapBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"- go.mod", "- main.go", "- web/index.ts", "Go: 1", "TypeScript: 1", "Entry points:", "Configuration:"} {
		if !strings.Contains(first.Content, expected) {
			t.Fatalf("map missing %q:\n%s", expected, first.Content)
		}
	}
	if strings.Contains(first.Content, "hidden.js") || first.Content != second.Content || first.Files != 3 {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
}

func TestBuildProjectMapByteLimitPreservesUTF8(t *testing.T) {
	workspace := t.TempDir()
	writeMapFile(t, filepath.Join(workspace, "中文.go"), "package demo")
	projectMap, err := BuildProjectMap(workspace, 80)
	if err != nil || len(projectMap.Content) > 80 || !strings.Contains(projectMap.Content, "truncated") {
		t.Fatalf("map=%#v err=%v", projectMap, err)
	}
}

func writeMapFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
