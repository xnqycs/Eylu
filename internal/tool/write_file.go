package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type WriteFile struct {
	paths      *pathResolver
	context    *CodeContext
	createOnly bool
}

func NewWriteFileWithContext(codeContext *CodeContext) *WriteFile {
	return &WriteFile{paths: codeContext.index.paths, context: codeContext}
}

func NewWriteFile(workspace string) (*WriteFile, error) {
	paths, err := newPathResolver(workspace)
	if err != nil {
		return nil, err
	}
	return &WriteFile{paths: paths}, nil
}

func (w *WriteFile) CreateOnly() *WriteFile {
	clone := *w
	clone.createOnly = true
	return &clone
}

func (w *WriteFile) Definition() protocol.ToolDefinition {
	description := "Atomically create or replace a UTF-8 file inside the workspace. For a requested standalone file, call this directly once the target path and content are clear. Use for complete file content after inspecting relevant related code. Parent directories are created only when create_parent_dirs is true. Existing file permissions are preserved."
	if w.createOnly {
		description = "Atomically create a new UTF-8 file inside the workspace. Existing paths return a conflict; read the file and use edit_file for precise changes. Parent directories are created only when create_parent_dirs is true."
	}
	return protocol.ToolDefinition{
		Name:        "write_file",
		Description: description,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"},"create_parent_dirs":{"type":"boolean","default":false},"reason":{"type":"string","minLength":1,"description":"User-facing reason"}},"required":["path","content","reason"],"additionalProperties":false}`),
	}
}

func (w *WriteFile) Risk() policy.Risk { return policy.RiskWrite }

// previousHashMaxBytes bounds how much of a file is read to record the intent's
// evidence. Above it the content is not hashed, because the intent is written
// before every side effect and hashing a large file would charge that cost to
// every write.
const previousHashMaxBytes = 4 << 20

// ReportIntent describes the recovery hints of one call without running it: the
// resolved path and the evidence of what the target held before.
//
// It is preparation, not execution: it creates no directory and writes no file.
// The path comes from the side-effect-free resolver because the parent directories
// of the write are created later, by the write itself, and only after this intent
// is durable.
//
// Below the size bound that evidence is the hash of the content, which is strong:
// if the content still matches, the operation provably did not change the target.
// Above it only the size and modification time are recorded, marked as weak.
// That evidence is weaker on purpose and the marker says so - a rewrite that
// preserves both size and timestamp (within the filesystem's granularity) leaves
// the same marker, so a match is a hint rather than proof, and recovery reports it
// as weak instead of drawing a firm conclusion from it.
func (w *WriteFile) ReportIntent(ctx context.Context, raw json.RawMessage) (string, string) {
	if ctx.Err() != nil {
		return "", ""
	}
	var input struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return "", ""
	}
	path, err := w.paths.writeTarget(input.Path)
	if err != nil {
		return input.Path, ""
	}
	return path, previousContentEvidence(ctx, path)
}

// weakEvidencePrefix marks evidence that is a size and a timestamp rather than a
// content hash, so a reader never mistakes it for a hash.
const weakEvidencePrefix = "weak:"

// previousContentEvidence returns what the target held before the call: the hash
// of its content when it is small enough to read, a weak size/timestamp marker
// when it is not, and nothing when it does not exist.
//
// The read is bounded by previousHashMaxBytes, so describing an intent cannot turn
// into an unbounded read of a large target, and it gives up when the request is
// cancelled: a request that will not write must not keep reading.
func previousContentEvidence(ctx context.Context, path string) string {
	if ctx.Err() != nil {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return ""
	}
	if info.Size() > previousHashMaxBytes {
		return fmt.Sprintf("%ssize=%d:mtime=%d", weakEvidencePrefix, info.Size(), info.ModTime().Unix())
	}
	return fileContentHash(ctx, path)
}

// fileContentHash returns the hash of a file's current content, or an empty
// string when it does not exist, cannot be read, or the request was cancelled
// while it was being read. It is evidence for recovery, never a precondition for
// writing.
func fileContentHash(ctx context.Context, path string) string {
	if ctx.Err() != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil || ctx.Err() != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (w *WriteFile) ClassifyConcurrency(raw json.RawMessage, _ policy.Outcome) ConcurrencySpec {
	var input struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return ConcurrencySpec{Mode: ConcurrencyExclusive}
	}
	path, err := w.paths.resourcePath(input.Path)
	if err != nil {
		return ConcurrencySpec{Mode: ConcurrencyExclusive}
	}
	return ConcurrencySpec{Mode: ConcurrencyClaimed, Claims: []ResourceClaim{{Kind: ResourceFile, Path: path, Access: ResourceWrite}}}
}

func (w *WriteFile) Execute(ctx context.Context, raw json.RawMessage) protocol.ToolResult {
	// Parse and read nothing before the cancellation is observed.
	if err := ctx.Err(); err != nil {
		return cancelledToolResult(err)
	}
	var input struct {
		Path             string `json:"path"`
		Content          string `json:"content"`
		CreateParentDirs bool   `json:"create_parent_dirs"`
		Reason           string `json:"reason"`
	}
	if err := decodeStrict(raw, &input); err != nil {
		return toolError("invalid write_file input: " + err.Error())
	}
	path, err := w.paths.forWrite(ctx, input.Path, input.CreateParentDirs)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return cancelledToolResult(ctxErr)
		}
		return toolError("resolve file: " + err.Error())
	}
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		if w.createOnly {
			return toolError("target already exists; read it and use edit_file")
		}
		if !info.Mode().IsRegular() {
			return toolError("target is not a regular file")
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return toolError(statErr.Error())
	}
	content := []byte(input.Content)
	previousHash := fileContentHash(ctx, path)
	if err := writeFileAtomically(ctx, path, content, mode); err != nil {
		// Once the atomic replace succeeded the write has happened, so a
		// cancellation observed afterwards must not be reported as a failure.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return cancelledToolResult(ctxErr)
		}
		return toolError(err.Error())
	}
	if w.context != nil {
		w.context.Invalidate(path)
	}
	lines := 0
	if len(content) > 0 {
		lines = strings.Count(input.Content, "\n") + 1
	}
	written := sha256.Sum256(content)
	return protocol.ToolResult{Content: fmt.Sprintf("wrote %d bytes to %s", len(content), input.Path), Metadata: map[string]any{
		"path": path, "bytes": len(content), "lines": lines,
		// Verifiable recovery hints: they describe what the file was and what it
		// became, so an interrupted run can be checked instead of guessed at.
		"file_hash": hex.EncodeToString(written[:]), "previous_hash": previousHash,
	}}
}

// writeFileAtomically writes content through a temporary file and an atomic
// replace, checking cancellation before each side-effecting boundary.
//
// Cancellation is cooperative: it is checked before the temporary file is
// created and again before the atomic replace. Once the replace succeeds the
// write has happened and the caller must report success rather than pretend the
// operation did not run. A cancelled attempt removes its temporary artifact.
func writeFileAtomically(ctx context.Context, path string, content []byte, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".eylu-write-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := replaceAtomically(temporaryPath, path); err != nil {
		return err
	}
	committed = true
	return nil
}
