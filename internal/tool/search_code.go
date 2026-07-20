package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type SearchCode struct {
	index        *RepositoryIndex
	maxResults   int
	maxFileBytes int64
}

type SearchMatch struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Text   string `json:"text"`
}

func NewSearchCode(index *RepositoryIndex, maxResults int, maxFileBytes int64) *SearchCode {
	if maxResults <= 0 {
		maxResults = 200
	}
	if maxFileBytes <= 0 {
		maxFileBytes = 2 << 20
	}
	return &SearchCode{index: index, maxResults: maxResults, maxFileBytes: maxFileBytes}
}

func (s *SearchCode) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{
		Name:        "search_code",
		Description: "Search text in the shared repository index. Supports literal or RE2 regular-expression matching, optional file glob, stable path/line ordering, result limits, binary skipping, and per-file size limits. Use it to locate symbols before reading or editing files.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"regex":{"type":"boolean","default":false},"glob":{"type":"string"},"max_results":{"type":"integer","minimum":1}},"required":["query"],"additionalProperties":false}`),
	}
}

func (s *SearchCode) Risk() policy.Risk { return policy.RiskRead }

func (s *SearchCode) ParallelSafe() bool { return true }

func (s *SearchCode) ClassifyConcurrency(_ json.RawMessage, _ policy.Outcome) ConcurrencySpec {
	resourcePath, err := s.index.paths.resourcePath(".")
	if err != nil {
		return ConcurrencySpec{Mode: ConcurrencyExclusive}
	}
	return ConcurrencySpec{Mode: ConcurrencyClaimed, Claims: []ResourceClaim{{Kind: ResourceTree, Path: resourcePath, Access: ResourceRead}}}
}

func (s *SearchCode) Execute(ctx context.Context, raw json.RawMessage) protocol.ToolResult {
	var input struct {
		Query      string `json:"query"`
		Regex      bool   `json:"regex"`
		Glob       string `json:"glob"`
		MaxResults int    `json:"max_results"`
	}
	if err := decodeStrict(raw, &input); err != nil {
		return toolError("invalid search_code input: " + err.Error())
	}
	if input.Query == "" {
		return toolError("query is required")
	}
	limit := input.MaxResults
	if limit <= 0 || limit > s.maxResults {
		limit = s.maxResults
	}
	var expression *regexp.Regexp
	if input.Regex {
		var err error
		expression, err = regexp.Compile(input.Query)
		if err != nil {
			return toolError("invalid regular expression: " + err.Error())
		}
	}
	snapshot := s.index.Refresh(ctx)
	matches := make([]SearchMatch, 0)
	skippedBinary, skippedLarge := 0, 0
	for _, file := range snapshot.Files {
		if input.Glob != "" && !matchFileGlob(input.Glob, file.Relative) {
			continue
		}
		if file.Size > s.maxFileBytes {
			skippedLarge++
			continue
		}
		openFile, err := os.Open(file.Absolute)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(openFile, s.maxFileBytes+1))
		_ = openFile.Close()
		if err != nil {
			continue
		}
		if int64(len(data)) > s.maxFileBytes {
			skippedLarge++
			continue
		}
		if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
			skippedBinary++
			continue
		}
		lines := strings.Split(string(data), "\n")
		for lineIndex, line := range lines {
			column := -1
			if expression != nil {
				location := expression.FindStringIndex(line)
				if location != nil {
					column = utf8.RuneCountInString(line[:location[0]]) + 1
				}
			} else if byteIndex := strings.Index(line, input.Query); byteIndex >= 0 {
				column = utf8.RuneCountInString(line[:byteIndex]) + 1
			}
			if column > 0 {
				matches = append(matches, SearchMatch{Path: file.Relative, Line: lineIndex + 1, Column: column, Text: strings.TrimSuffix(line, "\r")})
				if len(matches) >= limit {
					break
				}
			}
		}
		if len(matches) >= limit {
			break
		}
	}
	sort.SliceStable(matches, func(a, b int) bool {
		if matches[a].Path != matches[b].Path {
			return matches[a].Path < matches[b].Path
		}
		if matches[a].Line != matches[b].Line {
			return matches[a].Line < matches[b].Line
		}
		return matches[a].Column < matches[b].Column
	})
	payload, _ := json.MarshalIndent(map[string]any{
		"matches": matches, "truncated": len(matches) >= limit, "source": snapshot.Source,
		"skipped_binary": skippedBinary, "skipped_large": skippedLarge, "diagnostic": snapshot.Diagnostic,
	}, "", "  ")
	return protocol.ToolResult{Content: string(payload), Truncated: len(matches) >= limit, Metadata: map[string]any{"matches": len(matches), "source": snapshot.Source}}
}

func matchFileGlob(pattern, name string) bool {
	pattern = strings.TrimPrefix(path.Clean(strings.ReplaceAll(pattern, "\\", "/")), "./")
	name = strings.TrimPrefix(path.Clean(strings.ReplaceAll(name, "\\", "/")), "./")
	if !strings.Contains(pattern, "/") {
		matched, _ := path.Match(pattern, path.Base(name))
		return matched
	}
	return matchGlobSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchGlobSegments(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}
	if pattern[0] == "**" {
		if matchGlobSegments(pattern[1:], name) {
			return true
		}
		return len(name) > 0 && matchGlobSegments(pattern, name[1:])
	}
	if len(name) == 0 {
		return false
	}
	matched, err := path.Match(pattern[0], name[0])
	return err == nil && matched && matchGlobSegments(pattern[1:], name[1:])
}
