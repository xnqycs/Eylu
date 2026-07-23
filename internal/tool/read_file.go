package tool

import (
	"context"
	"encoding/json"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type ReadFile struct {
	paths    *pathResolver
	context  *CodeContext
	maxBytes int
	maxLines int
}

func NewReadFile(workspace string, maxBytes int) (*ReadFile, error) {
	codeContext, err := NewCodeContext(workspace, CodeContextOptions{})
	if err != nil {
		return nil, err
	}
	return NewReadFileWithContext(codeContext, maxBytes), nil
}

func NewReadFileWithContext(codeContext *CodeContext, maxBytes int) *ReadFile {
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	maxLines := defaultMaxReadLines
	if codeContext != nil {
		maxLines = codeContext.MaxReadLines()
	}
	return &ReadFile{paths: codeContext.index.paths, context: codeContext, maxBytes: maxBytes, maxLines: maxLines}
}

func (r *ReadFile) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{
		Name:        "read_file",
		Description: "Read an exact 1-based inclusive line range from a UTF-8 workspace file. Omit the range to read from line 1. Results include stable file and slice hashes plus the next line cursor when truncated.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Workspace-relative file path"},"start_line":{"type":"integer","minimum":1,"default":1},"end_line":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}`),
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

func (r *ReadFile) Execute(ctx context.Context, raw json.RawMessage) protocol.ToolResult {
	var input struct {
		Path      string `json:"path"`
		StartLine *int   `json:"start_line"`
		EndLine   *int   `json:"end_line"`
	}
	if err := decodeStrict(raw, &input); err != nil {
		return toolError("invalid read_file input: " + err.Error())
	}
	startLine, endLine := 1, 0
	if input.StartLine != nil {
		startLine = *input.StartLine
	}
	if input.EndLine != nil {
		endLine = *input.EndLine
	}
	slice, err := r.context.ReadSlice(ctx, input.Path, startLine, endLine, r.maxBytes)
	if err != nil {
		return toolError("read file: " + err.Error())
	}
	result := protocol.ToolResult{Content: slice.Content, Truncated: slice.Truncated, Metadata: map[string]any{
		"path": slice.AbsolutePath, "relative_path": slice.RelativePath, "bytes": slice.Bytes,
		"lines": slice.EndLine - slice.StartLine + 1, "lines_complete": !slice.Truncated,
		"total_lines": slice.TotalLines, "start_line": slice.StartLine, "end_line": slice.EndLine,
		"file_hash": slice.FileHash, "slice_hash": slice.SliceHash, "artifact_id": slice.ArtifactID,
		"next_start_line": slice.NextStartLine, "cache_hit": slice.CacheHit,
	}}
	if slice.TotalLines == 0 {
		result.Metadata["lines"] = 0
	}
	return result
}

func toolError(message string) protocol.ToolResult {
	return protocol.ToolResult{Content: message, IsError: true}
}
