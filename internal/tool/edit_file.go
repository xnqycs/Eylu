package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/pmezard/go-difflib/difflib"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type EditFile struct {
	paths    *pathResolver
	maxBytes int64
}

func NewEditFile(workspace string, maxBytes int64) (*EditFile, error) {
	paths, err := newPathResolver(workspace)
	if err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		maxBytes = 2 << 20
	}
	return &EditFile{paths: paths, maxBytes: maxBytes}, nil
}

func (e *EditFile) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{
		Name:        "edit_file",
		Description: "Replace an exact string in one UTF-8 workspace file after reading it. By default old_string must occur exactly once; expected_replacements can request another positive count. Matching failures leave the file unchanged and require a fresh read. Existing permissions and line-ending style are preserved. Returns a unified diff and line statistics.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"expected_replacements":{"type":"integer","minimum":1,"default":1},"reason":{"type":"string","minLength":1,"description":"User-facing reason"}},"required":["path","old_string","new_string","reason"],"additionalProperties":false}`),
	}
}

func (e *EditFile) Risk() policy.Risk { return policy.RiskWrite }

func (e *EditFile) ClassifyConcurrency(raw json.RawMessage, _ policy.Outcome) ConcurrencySpec {
	var input struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return ConcurrencySpec{Mode: ConcurrencyExclusive}
	}
	path, err := e.paths.resourcePath(input.Path)
	if err != nil {
		return ConcurrencySpec{Mode: ConcurrencyExclusive}
	}
	return ConcurrencySpec{Mode: ConcurrencyClaimed, Claims: []ResourceClaim{{Kind: ResourceFile, Path: path, Access: ResourceWrite}}}
}

func (e *EditFile) Execute(_ context.Context, raw json.RawMessage) protocol.ToolResult {
	var input struct {
		Path                 string `json:"path"`
		OldString            string `json:"old_string"`
		NewString            string `json:"new_string"`
		ExpectedReplacements *int   `json:"expected_replacements"`
		Reason               string `json:"reason"`
	}
	if err := decodeStrict(raw, &input); err != nil {
		return toolError("invalid edit_file input: " + err.Error())
	}
	if input.OldString == "" {
		return toolError("old_string must not be empty")
	}
	expected := 1
	if input.ExpectedReplacements != nil {
		expected = *input.ExpectedReplacements
	}
	if expected <= 0 {
		return toolError("expected_replacements must be greater than zero")
	}
	filePath, err := e.paths.existing(input.Path)
	if err != nil {
		return toolError("resolve file: " + err.Error())
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return toolError(err.Error())
	}
	if !info.Mode().IsRegular() {
		return toolError("path is not a regular file")
	}
	if info.Size() > e.maxBytes {
		return toolError(fmt.Sprintf("file exceeds edit limit of %d bytes", e.maxBytes))
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return toolError(err.Error())
	}
	if !utf8.Valid(data) {
		return toolError("file is not valid UTF-8 text")
	}
	original := string(data)
	lineEnding := detectLineEnding(original)
	oldString := normalizeLineEndings(input.OldString, lineEnding)
	newString := normalizeLineEndings(input.NewString, lineEnding)
	actual := strings.Count(original, oldString)
	if actual != expected {
		return toolError(fmt.Sprintf("old_string matched %d times; expected %d. Read the file again and provide a unique exact match", actual, expected))
	}
	updated := strings.Replace(original, oldString, newString, expected)
	diff, added, removed, err := unifiedDiff(input.Path, original, updated)
	if err != nil {
		return toolError("generate diff: " + err.Error())
	}
	if err := writeFileAtomically(filePath, []byte(updated), info.Mode().Perm()); err != nil {
		return toolError("write edit: " + err.Error())
	}
	content := fmt.Sprintf("replacements: %d\nlines_added: %d\nlines_removed: %d\n%s", actual, added, removed, diff)
	return protocol.ToolResult{Content: content, Metadata: map[string]any{
		"path": filePath, "replacements": actual, "lines_added": added, "lines_removed": removed, "diff": diff,
	}}
}

func detectLineEnding(content string) string {
	if strings.Contains(content, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

func normalizeLineEndings(content, lineEnding string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if lineEnding == "\r\n" {
		content = strings.ReplaceAll(content, "\n", "\r\n")
	}
	return content
}

func unifiedDiff(name, before, after string) (string, int, int, error) {
	diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(before), B: difflib.SplitLines(after),
		FromFile: "a/" + strings.ReplaceAll(name, "\\", "/"), ToFile: "b/" + strings.ReplaceAll(name, "\\", "/"), Context: 3, Eol: "\n",
	})
	if err != nil {
		return "", 0, 0, err
	}
	added, removed := 0, 0
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			added++
		}
		if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			removed++
		}
	}
	return diff, added, removed, nil
}
