package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	context      *CodeContext
	maxResults   int
	maxFileBytes int64
}

type SearchMatch struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Column    int    `json:"column"`
	Text      string `json:"text"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Context   string `json:"context,omitempty"`
	FileHash  string `json:"file_hash"`
	SliceHash string `json:"slice_hash"`
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

func NewSearchCodeWithContext(codeContext *CodeContext, maxResults int, maxFileBytes int64) *SearchCode {
	search := NewSearchCode(codeContext.RepositoryIndex(), maxResults, maxFileBytes)
	search.context = codeContext
	return search
}

func (s *SearchCode) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{
		Name:        "search_code",
		Description: "Search the session's incremental lexical code index. Supports literal or RE2 matching, file globs, stable pagination, and optional surrounding lines. Results include file and slice hashes for precise follow-up reads.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"regex":{"type":"boolean","default":false},"glob":{"type":"string"},"max_results":{"type":"integer","minimum":1},"offset":{"type":"integer","minimum":0,"default":0},"context_lines":{"type":"integer","minimum":0,"maximum":20,"default":0}},"required":["query"],"additionalProperties":false}`),
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
		Query        string `json:"query"`
		Regex        bool   `json:"regex"`
		Glob         string `json:"glob"`
		MaxResults   int    `json:"max_results"`
		Offset       int    `json:"offset"`
		ContextLines int    `json:"context_lines"`
	}
	if err := decodeStrict(raw, &input); err != nil {
		return toolError("invalid search_code input: " + err.Error())
	}
	if input.Query == "" {
		return toolError("query is required")
	}
	if input.Offset < 0 {
		return toolError("offset cannot be negative")
	}
	if input.ContextLines < 0 || input.ContextLines > 20 {
		return toolError("context_lines must be between 0 and 20")
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
	codeContext := s.context
	if codeContext == nil {
		codeContext = newCodeContext(s.index, CodeContextOptions{})
	}
	regexPrefix := ""
	if expression != nil {
		regexPrefix, _ = expression.LiteralPrefix()
	}
	snapshot, generation, candidates, err := codeContext.CandidateFiles(ctx, input.Query, regexPrefix, s.maxFileBytes)
	if err != nil {
		return toolError("search index: " + err.Error())
	}
	matches := make([]SearchMatch, 0, limit+1)
	skippedBinary, skippedLarge := codeContext.LexicalStats(generation, s.maxFileBytes)
	cacheHits := 0
	seen := 0
	for _, file := range candidates {
		if input.Glob != "" && !matchFileGlob(input.Glob, file.Relative) {
			continue
		}
		if file.Size > s.maxFileBytes {
			skippedLarge++
			continue
		}
		data, fileHash, binary, cacheHit, err := codeContext.FileText(ctx, file)
		if err != nil {
			continue
		}
		if cacheHit {
			cacheHits++
		}
		if binary {
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
				if seen < input.Offset {
					seen++
					continue
				}
				startLine := lineIndex + 1 - input.ContextLines
				if startLine < 1 {
					startLine = 1
				}
				endLine := lineIndex + 1 + input.ContextLines
				if endLine > len(lines) {
					endLine = len(lines)
				}
				contextText := strings.Join(lines[startLine-1:endLine], "\n")
				digest := sha256.Sum256([]byte(contextText))
				matches = append(matches, SearchMatch{
					Path: file.Relative, Line: lineIndex + 1, Column: column, Text: strings.TrimSuffix(line, "\r"),
					StartLine: startLine, EndLine: endLine, Context: contextText, FileHash: fileHash,
					SliceHash: hex.EncodeToString(digest[:]),
				})
				if len(matches) > limit {
					break
				}
			}
		}
		if len(matches) > limit {
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
	truncated := len(matches) > limit
	if truncated {
		matches = matches[:limit]
	}
	nextOffset := 0
	if truncated {
		nextOffset = input.Offset + len(matches)
	}
	payload, _ := json.MarshalIndent(map[string]any{
		"matches": matches, "truncated": truncated, "next_offset": nextOffset,
		"index_generation": generation, "source": snapshot.Source, "cache_hits": cacheHits,
		"skipped_binary": skippedBinary, "skipped_large": skippedLarge, "diagnostic": snapshot.Diagnostic,
	}, "", "  ")
	return protocol.ToolResult{Content: string(payload), Truncated: truncated, Metadata: map[string]any{
		"matches": len(matches), "source": snapshot.Source, "next_offset": nextOffset,
		"index_generation": generation, "cache_hits": cacheHits,
	}}
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
