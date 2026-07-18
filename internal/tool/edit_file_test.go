package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEditFileUniqueMatchDiffAndPermissions(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "main.go")
	original := "package main\n\nfunc value() string {\n\treturn \"old\"\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	editor, err := NewEditFile(workspace, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	result := editor.Execute(context.Background(), mustJSON(t, map[string]any{
		"path": "main.go", "old_string": "return \"old\"", "new_string": "return \"new\"",
	}))
	if result.IsError || !strings.Contains(result.Content, "--- a/main.go") || !strings.Contains(result.Content, "+\treturn \"new\"") {
		t.Fatalf("result = %#v", result)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "return \"new\"") || strings.Contains(string(data), "return \"old\"") {
		t.Fatalf("file = %q, %v", data, err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o640 {
			t.Fatalf("mode = %o", info.Mode().Perm())
		}
	}
}

func TestEditFileMismatchLeavesFileAndSupportsEmptyReplacement(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "values.txt")
	if err := os.WriteFile(path, []byte("same\nsame\nremove\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	editor, _ := NewEditFile(workspace, 1024)
	for _, input := range []map[string]any{
		{"path": "values.txt", "old_string": "same", "new_string": "other"},
		{"path": "values.txt", "old_string": "missing", "new_string": "other"},
	} {
		result := editor.Execute(context.Background(), mustJSON(t, input))
		if !result.IsError || !strings.Contains(result.Content, "Read the file again") {
			t.Fatalf("result = %#v", result)
		}
	}
	data, _ := os.ReadFile(path)
	if string(data) != "same\nsame\nremove\n" {
		t.Fatalf("file changed after mismatch: %q", data)
	}
	result := editor.Execute(context.Background(), mustJSON(t, map[string]any{
		"path": "values.txt", "old_string": "remove\n", "new_string": "", "expected_replacements": 1,
	}))
	if result.IsError {
		t.Fatalf("remove result = %#v", result)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "same\nsame\n" {
		t.Fatalf("file = %q", data)
	}
}

func TestEditFilePreservesCRLFAndRejectsEncodingAndLimits(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "crlf.txt"), []byte("one\r\ntwo\r\nthree\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	editor, _ := NewEditFile(workspace, 64)
	result := editor.Execute(context.Background(), mustJSON(t, map[string]any{
		"path": "crlf.txt", "old_string": "two\nthree", "new_string": "second\nthird",
	}))
	if result.IsError {
		t.Fatalf("CRLF result = %#v", result)
	}
	data, _ := os.ReadFile(filepath.Join(workspace, "crlf.txt"))
	if string(data) != "one\r\nsecond\r\nthird\r\n" {
		t.Fatalf("CRLF file = %q", data)
	}
	if err := os.WriteFile(filepath.Join(workspace, "invalid.bin"), []byte{0xff, 0xfe}, 0o600); err != nil {
		t.Fatal(err)
	}
	result = editor.Execute(context.Background(), mustJSON(t, map[string]any{"path": "invalid.bin", "old_string": "x", "new_string": "y"}))
	if !result.IsError || !strings.Contains(result.Content, "UTF-8") {
		t.Fatalf("encoding result = %#v", result)
	}
	if err := os.WriteFile(filepath.Join(workspace, "large.txt"), []byte(strings.Repeat("x", 65)), 0o600); err != nil {
		t.Fatal(err)
	}
	result = editor.Execute(context.Background(), mustJSON(t, map[string]any{"path": "large.txt", "old_string": "x", "new_string": "y", "expected_replacements": 65}))
	if !result.IsError || !strings.Contains(result.Content, "edit limit") {
		t.Fatalf("large result = %#v", result)
	}
	result = editor.Execute(context.Background(), mustJSON(t, map[string]any{"path": "../outside.txt", "old_string": "x", "new_string": "y"}))
	if !result.IsError || !strings.Contains(result.Content, "outside workspace") {
		t.Fatalf("traversal result = %#v", result)
	}
}

func TestEditFileRejectsEmptyOldString(t *testing.T) {
	editor, _ := NewEditFile(t.TempDir(), 1024)
	result := editor.Execute(context.Background(), json.RawMessage(`{"path":"file","old_string":"","new_string":"x"}`))
	if !result.IsError || !strings.Contains(result.Content, "must not be empty") {
		t.Fatalf("result = %#v", result)
	}
}
