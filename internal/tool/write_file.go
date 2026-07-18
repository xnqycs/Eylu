package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type WriteFile struct {
	paths *pathResolver
}

func NewWriteFile(workspace string) (*WriteFile, error) {
	paths, err := newPathResolver(workspace)
	if err != nil {
		return nil, err
	}
	return &WriteFile{paths: paths}, nil
}

func (w *WriteFile) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{
		Name:        "write_file",
		Description: "Atomically create or replace a UTF-8 file inside the workspace. Use for complete file content after inspecting related code. Parent directories are created only when create_parent_dirs is true. Existing file permissions are preserved.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"},"create_parent_dirs":{"type":"boolean","default":false}},"required":["path","content"],"additionalProperties":false}`),
	}
}

func (w *WriteFile) Risk() policy.Risk { return policy.RiskWrite }

func (w *WriteFile) Execute(_ context.Context, raw json.RawMessage) protocol.ToolResult {
	var input struct {
		Path             string `json:"path"`
		Content          string `json:"content"`
		CreateParentDirs bool   `json:"create_parent_dirs"`
	}
	if err := decodeStrict(raw, &input); err != nil {
		return toolError("invalid write_file input: " + err.Error())
	}
	path, err := w.paths.forWrite(input.Path, input.CreateParentDirs)
	if err != nil {
		return toolError("resolve file: " + err.Error())
	}
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		if !info.Mode().IsRegular() {
			return toolError("target is not a regular file")
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return toolError(statErr.Error())
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".eylu-write-*.tmp")
	if err != nil {
		return toolError(err.Error())
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return toolError(err.Error())
	}
	if _, err := temporary.WriteString(input.Content); err != nil {
		temporary.Close()
		return toolError(err.Error())
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return toolError(err.Error())
	}
	if err := temporary.Close(); err != nil {
		return toolError(err.Error())
	}
	if err := replaceAtomically(temporaryPath, path); err != nil {
		return toolError(err.Error())
	}
	return protocol.ToolResult{Content: fmt.Sprintf("wrote %d bytes to %s", len([]byte(input.Content)), input.Path), Metadata: map[string]any{"path": path, "bytes": len([]byte(input.Content))}}
}
