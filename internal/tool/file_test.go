package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestReadFileRangesHashesAndCacheInvalidation(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "source.txt")
	if err := os.WriteFile(path, []byte("alpha\r\nbeta\r\ngamma\r\ndelta"), 0o600); err != nil {
		t.Fatal(err)
	}
	codeContext, err := NewCodeContext(workspace, CodeContextOptions{MaxReadLines: 2, RefreshInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	reader := NewReadFileWithContext(codeContext, 1024)
	first := reader.Execute(context.Background(), json.RawMessage(`{"path":"source.txt","start_line":2,"end_line":4}`))
	if first.IsError || first.Content != "beta\r\ngamma\r\n" || !first.Truncated {
		t.Fatalf("first range = %#v", first)
	}
	if first.Metadata["start_line"] != 2 || first.Metadata["end_line"] != 3 || first.Metadata["total_lines"] != 4 || first.Metadata["next_start_line"] != 4 {
		t.Fatalf("first metadata = %#v", first.Metadata)
	}
	if first.Metadata["file_hash"] == "" || first.Metadata["slice_hash"] == "" || first.Metadata["artifact_id"] == "" || first.Metadata["cache_hit"] != false {
		t.Fatalf("first hashes = %#v", first.Metadata)
	}
	second := reader.Execute(context.Background(), json.RawMessage(`{"path":"source.txt","start_line":2,"end_line":3}`))
	if second.IsError || second.Metadata["cache_hit"] != true || second.Metadata["slice_hash"] != first.Metadata["slice_hash"] {
		t.Fatalf("cached range = %#v", second)
	}

	writer := NewWriteFileWithContext(codeContext)
	written := writer.Execute(context.Background(), json.RawMessage(`{"path":"source.txt","content":"changed\ncontent"}`))
	if written.IsError {
		t.Fatalf("write = %#v", written)
	}
	after := reader.Execute(context.Background(), json.RawMessage(`{"path":"source.txt"}`))
	if after.IsError || after.Metadata["cache_hit"] != false || after.Metadata["file_hash"] == first.Metadata["file_hash"] {
		t.Fatalf("invalidated range = %#v", after)
	}
}

func TestCodeContextRefreshIsSingleFlight(t *testing.T) {
	workspace := t.TempDir()
	var calls atomic.Int32
	index, err := newRepositoryIndex(workspace, func(context.Context, string, ...string) ([]byte, error) {
		calls.Add(1)
		time.Sleep(10 * time.Millisecond)
		return nil, os.ErrNotExist
	})
	if err != nil {
		t.Fatal(err)
	}
	codeContext := newCodeContext(index, CodeContextOptions{RefreshInterval: time.Hour})
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			codeContext.Refresh(context.Background())
		}()
	}
	wait.Wait()
	if calls.Load() != 1 {
		t.Fatalf("git probes = %d, want 1", calls.Load())
	}
}

func TestCodeContextFileLoadIsSingleFlight(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "source.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	codeContext, err := NewCodeContext(workspace, CodeContextOptions{RefreshInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	originalRead := codeContext.readFile
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	codeContext.readFile = func(name string) ([]byte, error) {
		calls.Add(1)
		startOnce.Do(func() { close(started) })
		<-release
		return originalRead(name)
	}

	const readers = 16
	ready := make(chan struct{}, readers)
	begin := make(chan struct{})
	errors := make(chan error, readers)
	for range readers {
		go func() {
			ready <- struct{}{}
			<-begin
			_, err := codeContext.ReadSlice(context.Background(), "source.txt", 1, 1, 1024)
			errors <- err
		}()
	}
	for range readers {
		<-ready
	}
	close(begin)
	<-started
	time.Sleep(20 * time.Millisecond)
	close(release)
	for range readers {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("file reads = %d, want 1", calls.Load())
	}
}

func TestReadFileByteTruncationReportsCompletedLineBoundary(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "lines.txt"), []byte("one\ntwo\nthree"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReadFile(workspace, 5)
	if err != nil {
		t.Fatal(err)
	}
	result := reader.Execute(context.Background(), json.RawMessage(`{"path":"lines.txt"}`))
	if result.IsError || !result.Truncated || result.Content != "one\n" || result.Metadata["end_line"] != 1 || result.Metadata["next_start_line"] != 2 {
		t.Fatalf("result = %#v", result)
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
