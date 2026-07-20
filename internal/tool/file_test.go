package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"Eylu/internal/protocol"
)

func TestReadAndWriteFileBoundaries(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "source.txt"), []byte("hello world"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReadFile(workspace, 5)
	if err != nil {
		t.Fatal(err)
	}
	result := reader.Execute(context.Background(), json.RawMessage(`{"path":"source.txt"}`))
	if result.IsError || !result.Truncated || !strings.HasPrefix(result.Content, "hello") {
		t.Fatalf("read result = %#v", result)
	}
	if result.Metadata["bytes"] != int64(11) || result.Metadata["lines"] != 1 || result.Metadata["lines_complete"] != false {
		t.Fatalf("read metadata = %#v", result.Metadata)
	}
	traversal := reader.Execute(context.Background(), mustJSON(t, map[string]any{"path": filepath.Join("..", filepath.Base(outside), "secret.txt")}))
	if !traversal.IsError || !strings.Contains(traversal.Content, "outside workspace") {
		t.Fatalf("traversal result = %#v", traversal)
	}
	if err := os.WriteFile(filepath.Join(workspace, "binary.bin"), []byte{0xff, 0xfe}, 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := reader.Execute(context.Background(), json.RawMessage(`{"path":"binary.bin"}`))
	if !invalid.IsError || !strings.Contains(invalid.Content, "UTF-8") {
		t.Fatalf("invalid UTF-8 result = %#v", invalid)
	}

	writer, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	missingParent := writer.Execute(context.Background(), json.RawMessage(`{"path":"nested/new.txt","content":"value"}`))
	if !missingParent.IsError {
		t.Fatal("write should require explicit parent creation")
	}
	written := writer.Execute(context.Background(), json.RawMessage(`{"path":"nested/new.txt","content":"value","create_parent_dirs":true}`))
	if written.IsError {
		t.Fatalf("write result = %#v", written)
	}
	if written.Metadata["bytes"] != 5 || written.Metadata["lines"] != 1 {
		t.Fatalf("write metadata = %#v", written.Metadata)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "nested", "new.txt"))
	if err != nil || string(data) != "value" {
		t.Fatalf("file = %q, %v", data, err)
	}
	replaced := writer.Execute(context.Background(), json.RawMessage(`{"path":"source.txt","content":"replacement"}`))
	if replaced.IsError {
		t.Fatalf("replace result = %#v", replaced)
	}
	data, _ = os.ReadFile(filepath.Join(workspace, "source.txt"))
	if string(data) != "replacement" {
		t.Fatalf("replaced file = %q", data)
	}
}

func TestReadRejectsSymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workspace, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	reader, err := NewReadFile(workspace, 1024)
	if err != nil {
		t.Fatal(err)
	}
	result := reader.Execute(context.Background(), json.RawMessage(`{"path":"link.txt"}`))
	if !result.IsError || !strings.Contains(result.Content, "outside workspace") {
		t.Fatalf("result = %#v", result)
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

var _ protocol.ToolResult
