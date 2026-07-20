package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type ReadFile struct {
	paths    *pathResolver
	maxBytes int
}

func NewReadFile(workspace string, maxBytes int) (*ReadFile, error) {
	paths, err := newPathResolver(workspace)
	if err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	return &ReadFile{paths: paths, maxBytes: maxBytes}, nil
}

func (r *ReadFile) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{
		Name:        "read_file",
		Description: "Read a UTF-8 text file inside the workspace. Use it before editing or when exact source content is needed. Directories and paths that escape through traversal or symlinks are rejected. Large files are truncated to the configured byte limit.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Workspace-relative file path"}},"required":["path"],"additionalProperties":false}`),
	}
}

func (r *ReadFile) Risk() policy.Risk { return policy.RiskRead }

func (r *ReadFile) ParallelSafe() bool { return true }

func (r *ReadFile) ClassifyConcurrency(raw json.RawMessage, _ policy.Outcome) ConcurrencySpec {
	var input struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return ConcurrencySpec{Mode: ConcurrencyExclusive}
	}
	path, err := r.paths.resourcePath(input.Path)
	if err != nil {
		return ConcurrencySpec{Mode: ConcurrencyExclusive}
	}
	return ConcurrencySpec{Mode: ConcurrencyClaimed, Claims: []ResourceClaim{{Kind: ResourceFile, Path: path, Access: ResourceRead}}}
}

func (r *ReadFile) Execute(_ context.Context, raw json.RawMessage) protocol.ToolResult {
	var input struct {
		Path string `json:"path"`
	}
	if err := decodeStrict(raw, &input); err != nil {
		return toolError("invalid read_file input: " + err.Error())
	}
	path, err := r.paths.existing(input.Path)
	if err != nil {
		return toolError("resolve file: " + err.Error())
	}
	info, err := os.Stat(path)
	if err != nil {
		return toolError(err.Error())
	}
	if !info.Mode().IsRegular() {
		return toolError("path is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return toolError(err.Error())
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(r.maxBytes)+1))
	if err != nil {
		return toolError(err.Error())
	}
	truncated := len(data) > r.maxBytes
	if truncated {
		data = data[:r.maxBytes]
	}
	if !utf8.Valid(data) {
		return toolError("file is not valid UTF-8 text")
	}
	lines := 0
	if len(data) > 0 {
		lines = strings.Count(string(data), "\n") + 1
	}
	result := protocol.ToolResult{Content: string(data), Truncated: truncated, Metadata: map[string]any{
		"path": path, "bytes": info.Size(), "lines": lines, "lines_complete": !truncated,
	}}
	if truncated {
		result.Content += fmt.Sprintf("\n[read_file truncated at %d bytes]", r.maxBytes)
	}
	return result
}

func toolError(message string) protocol.ToolResult {
	return protocol.ToolResult{Content: message, IsError: true}
}
