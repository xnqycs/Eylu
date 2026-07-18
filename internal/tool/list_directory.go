package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type ListDirectory struct {
	index      *RepositoryIndex
	maxEntries int
}

func NewListDirectory(index *RepositoryIndex, maxEntries int) *ListDirectory {
	if maxEntries <= 0 {
		maxEntries = 2000
	}
	return &ListDirectory{index: index, maxEntries: maxEntries}
}

func (l *ListDirectory) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{
		Name:        "list_directory",
		Description: "Render a stable directory tree from the shared repository index. Supports a workspace-relative root, depth, hidden-file toggle, and entry limit. Git metadata and ignored files stay excluded by the index.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","default":"."},"depth":{"type":"integer","minimum":0,"default":2},"include_hidden":{"type":"boolean","default":false},"max_entries":{"type":"integer","minimum":1}},"additionalProperties":false}`),
	}
}

func (l *ListDirectory) Risk() policy.Risk { return policy.RiskRead }

func (l *ListDirectory) Execute(ctx context.Context, raw json.RawMessage) protocol.ToolResult {
	var input struct {
		Path          string `json:"path"`
		Depth         *int   `json:"depth"`
		IncludeHidden bool   `json:"include_hidden"`
		MaxEntries    int    `json:"max_entries"`
	}
	if err := decodeStrict(raw, &input); err != nil {
		return toolError("invalid list_directory input: " + err.Error())
	}
	root := strings.Trim(strings.ReplaceAll(input.Path, "\\", "/"), "/")
	if root == "" || root == "." {
		root = ""
	}
	if root == ".." || strings.HasPrefix(root, "../") || path.IsAbs(root) {
		return toolError("path is outside workspace")
	}
	directoryPath := l.index.workspace
	if root != "" {
		var err error
		directoryPath, err = l.index.paths.existing(root)
		if err != nil {
			return toolError("resolve directory: " + err.Error())
		}
	}
	info, err := os.Stat(directoryPath)
	if err != nil || !info.IsDir() {
		return toolError("path is not a directory")
	}
	depth := 2
	if input.Depth != nil {
		depth = *input.Depth
	}
	if depth < 0 {
		return toolError("depth cannot be negative")
	}
	limit := input.MaxEntries
	if limit <= 0 || limit > l.maxEntries {
		limit = l.maxEntries
	}
	snapshot := l.index.Refresh(ctx)
	entries := make(map[string]bool)
	for _, file := range snapshot.Files {
		relative := file.Relative
		if root != "" {
			prefix := root + "/"
			if relative != root && !strings.HasPrefix(relative, prefix) {
				continue
			}
			relative = strings.TrimPrefix(relative, prefix)
		}
		segments := strings.Split(relative, "/")
		if !input.IncludeHidden && containsHidden(segments) {
			continue
		}
		if len(segments) > depth+1 {
			segments = segments[:depth+1]
		}
		for index := range segments {
			entry := strings.Join(segments[:index+1], "/")
			isDirectory := index < len(segments)-1 || len(strings.Split(relative, "/")) > len(segments)
			entries[entry] = entries[entry] || isDirectory
		}
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	truncated := len(names) > limit
	if truncated {
		names = names[:limit]
	}
	var tree strings.Builder
	rootLabel := "."
	if root != "" {
		rootLabel = root
	}
	fmt.Fprintln(&tree, rootLabel)
	for _, name := range names {
		segments := strings.Split(name, "/")
		indent := strings.Repeat("  ", len(segments))
		label := segments[len(segments)-1]
		if entries[name] {
			label += "/"
		}
		fmt.Fprintf(&tree, "%s%s\n", indent, label)
	}
	if truncated {
		fmt.Fprintf(&tree, "[directory listing truncated at %d entries]\n", limit)
	}
	return protocol.ToolResult{Content: tree.String(), Truncated: truncated, Metadata: map[string]any{"entries": len(names), "source": snapshot.Source, "diagnostic": snapshot.Diagnostic}}
}

func containsHidden(segments []string) bool {
	for _, segment := range segments {
		if strings.HasPrefix(segment, ".") && segment != "." && segment != ".." {
			return true
		}
	}
	return false
}
