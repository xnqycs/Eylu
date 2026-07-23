package context

import (
	"encoding/json"
	"fmt"
	"strings"

	"Eylu/internal/protocol"
)

type PromptResult struct {
	Turns      []protocol.Turn
	Tools      []protocol.ToolDefinition
	Blocks     []Block
	SliceStats SliceStats
}

type SliceStats struct {
	CacheHits    int `json:"cache_hits"`
	Deduplicated int `json:"deduplicated"`
	Stale        int `json:"stale"`
}

func (r PromptResult) InputTokens() int {
	total := 0
	for _, block := range r.Blocks {
		if block.Category != CategoryOutputReserve {
			total += block.Tokens
		}
	}
	return total
}

type PromptBuilder struct {
	estimator  TokenEstimator
	result     PromptResult
	slices     []canonicalSlice
	references []sliceReferenceRecord
}

type sliceReferenceRecord struct {
	canonicalID string
	path        string
	fileHash    string
	startLine   int
	endLine     int
	turnIndex   int
	partIndex   int
	blockIndex  int
}

type canonicalSlice struct {
	path       string
	fileHash   string
	artifactID string
	startLine  int
	endLine    int
	turnIndex  int
	partIndex  int
	blockIndex int
}

func NewPromptBuilder(estimator TokenEstimator) *PromptBuilder {
	if estimator == nil {
		estimator = ApproxEstimator{BytesPerToken: 4}
	}
	return &PromptBuilder{estimator: estimator}
}

func (b *PromptBuilder) AddTextTurn(id string, role protocol.Role, text string, category Category, source string, protected bool, metadata map[string]any) {
	if text == "" {
		return
	}
	b.result.Turns = append(b.result.Turns, protocol.Turn{ID: id, Role: role, Parts: []protocol.Part{{Kind: protocol.PartText, Text: text}}})
	b.addTextBlock(id+":0", category, source, text, protected, metadata)
}

func (b *PromptBuilder) AddTurn(turn protocol.Turn) {
	turn.Parts = append([]protocol.Part(nil), turn.Parts...)
	for index, part := range turn.Parts {
		if part.ToolResult != nil {
			result := *part.ToolResult
			result.Metadata = clonePromptMetadata(part.ToolResult.Metadata)
			turn.Parts[index].ToolResult = &result
		}
	}
	b.result.Turns = append(b.result.Turns, turn)
	turnIndex := len(b.result.Turns) - 1
	for index, part := range turn.Parts {
		id := fmt.Sprintf("%s:%d", turn.ID, index)
		switch {
		case part.Kind == protocol.PartText || part.Kind == protocol.PartReasoning:
			category := CategoryAgentMessage
			if turn.Role == protocol.RoleUser {
				category = CategoryUserMessage
			}
			b.addTextBlock(id, category, turn.ID, part.Text, false, map[string]any{"part_kind": part.Kind})
		case part.Kind == protocol.PartToolCall && part.ToolCall != nil:
			content := part.ToolCall.Name + "\n" + string(part.ToolCall.Arguments)
			b.addTextBlock(id, CategoryAgentMessage, part.ToolCall.Name, content, false, map[string]any{"call_id": part.ToolCall.ID, "part_kind": part.Kind})
		case part.Kind == protocol.PartToolResult && part.ToolResult != nil:
			category, source := toolResultCategory(part.ToolResult)
			metadata := map[string]any{"call_id": part.ToolResult.CallID, "is_error": part.ToolResult.IsError, "truncated": part.ToolResult.Truncated}
			content := part.ToolResult.Content
			if codeSlice, ok := parseCodeSlice(part.ToolResult.Metadata); ok && !part.ToolResult.IsError {
				category, source = CategoryCodeSlice, codeSlice.path
				for key, value := range codeSlice.metadata() {
					metadata[key] = value
				}
				var canonicalID string
				var stale bool
				content, canonicalID, stale = b.deduplicateSlice(codeSlice, turnIndex, index, content)
				b.result.Turns[turnIndex].Parts[index].ToolResult.Content = content
				metadata["canonical_artifact_id"] = canonicalID
				metadata["stale"] = stale
			}
			b.addTextBlock(id, category, source, content, false, metadata)
			if codeSlice, ok := parseCodeSlice(part.ToolResult.Metadata); ok {
				canonicalID, _ := metadata["canonical_artifact_id"].(string)
				if canonicalID == "" {
					b.slices = append(b.slices, canonicalSlice{
						path: codeSlice.path, fileHash: codeSlice.fileHash, artifactID: codeSlice.artifactID,
						startLine: codeSlice.startLine, endLine: codeSlice.endLine,
						turnIndex: turnIndex, partIndex: index, blockIndex: len(b.result.Blocks) - 1,
					})
				} else {
					b.references = append(b.references, sliceReferenceRecord{
						canonicalID: canonicalID, path: codeSlice.path, fileHash: codeSlice.fileHash,
						startLine: codeSlice.startLine, endLine: codeSlice.endLine,
						turnIndex: turnIndex, partIndex: index, blockIndex: len(b.result.Blocks) - 1,
					})
				}
			}
		}
	}
}

type codeSliceMetadata struct {
	path       string
	fileHash   string
	artifactID string
	startLine  int
	endLine    int
	cacheHit   bool
}

func (s codeSliceMetadata) metadata() map[string]any {
	return map[string]any{
		"path": s.path, "file_hash": s.fileHash, "artifact_id": s.artifactID,
		"start_line": s.startLine, "end_line": s.endLine, "cache_hit": s.cacheHit,
	}
}

func parseCodeSlice(metadata map[string]any) (codeSliceMetadata, bool) {
	if metadata == nil {
		return codeSliceMetadata{}, false
	}
	path, _ := metadata["relative_path"].(string)
	if path == "" {
		path, _ = metadata["path"].(string)
	}
	fileHash, _ := metadata["file_hash"].(string)
	artifactID, _ := metadata["artifact_id"].(string)
	startLine, startOK := promptInt(metadata["start_line"])
	endLine, endOK := promptInt(metadata["end_line"])
	cacheHit, _ := metadata["cache_hit"].(bool)
	if path == "" || fileHash == "" || artifactID == "" || !startOK || !endOK || startLine <= 0 || endLine < startLine {
		return codeSliceMetadata{}, false
	}
	return codeSliceMetadata{path: path, fileHash: fileHash, artifactID: artifactID, startLine: startLine, endLine: endLine, cacheHit: cacheHit}, true
}

func promptInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), typed == float64(int(typed))
	default:
		return 0, false
	}
}

func (b *PromptBuilder) deduplicateSlice(current codeSliceMetadata, turnIndex, partIndex int, content string) (string, string, bool) {
	if current.cacheHit {
		b.result.SliceStats.CacheHits++
	}
	stale := false
	var containing *canonicalSlice
	for _, existing := range b.slices {
		if existing.path == current.path && existing.fileHash != current.fileHash {
			stale = true
		}
		if existing.path == current.path && existing.fileHash == current.fileHash && existing.startLine <= current.startLine && existing.endLine >= current.endLine {
			copy := existing
			containing = &copy
		}
	}
	if stale {
		b.result.SliceStats.Stale++
	}
	if containing != nil {
		b.result.SliceStats.Deduplicated++
		return sliceReference(containing.artifactID, current.path, current.startLine, current.endLine, current.fileHash), containing.artifactID, stale
	}
	kept := b.slices[:0]
	for _, existing := range b.slices {
		if existing.path == current.path && existing.fileHash == current.fileHash && current.startLine <= existing.startLine && current.endLine >= existing.endLine {
			for index := range b.references {
				reference := &b.references[index]
				if reference.canonicalID != existing.artifactID {
					continue
				}
				reference.canonicalID = current.artifactID
				updated := sliceReference(current.artifactID, reference.path, reference.startLine, reference.endLine, reference.fileHash)
				b.result.Turns[reference.turnIndex].Parts[reference.partIndex].ToolResult.Content = updated
				block := &b.result.Blocks[reference.blockIndex]
				block.Bytes = len([]byte(updated))
				block.Tokens = b.estimator.Estimate(updated)
				block.Metadata["canonical_artifact_id"] = current.artifactID
			}
			reference := sliceReference(current.artifactID, existing.path, existing.startLine, existing.endLine, existing.fileHash)
			result := b.result.Turns[existing.turnIndex].Parts[existing.partIndex].ToolResult
			result.Content = reference
			block := &b.result.Blocks[existing.blockIndex]
			block.Bytes = len([]byte(reference))
			block.Tokens = b.estimator.Estimate(reference)
			block.Metadata["deduplicated"] = true
			block.Metadata["canonical_artifact_id"] = current.artifactID
			b.references = append(b.references, sliceReferenceRecord{
				canonicalID: current.artifactID, path: existing.path, fileHash: existing.fileHash,
				startLine: existing.startLine, endLine: existing.endLine,
				turnIndex: existing.turnIndex, partIndex: existing.partIndex, blockIndex: existing.blockIndex,
			})
			b.result.SliceStats.Deduplicated++
			continue
		}
		kept = append(kept, existing)
	}
	b.slices = kept
	return content, "", stale
}

func sliceReference(artifactID, path string, startLine, endLine int, fileHash string) string {
	return fmt.Sprintf("[code slice reference: artifact_id=%s path=%s lines=%d-%d file_hash=%s]", artifactID, path, startLine, endLine, fileHash)
}

func clonePromptMetadata(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func (b *PromptBuilder) AddTools(definitions []protocol.ToolDefinition, category Category, sourcePrefix string) {
	for _, definition := range definitions {
		b.result.Tools = append(b.result.Tools, definition)
		encoded, _ := json.Marshal(definition)
		source := definition.Name
		if sourcePrefix != "" {
			source = sourcePrefix + ":" + definition.Name
		}
		b.addTextBlock("tool-schema:"+source, category, source, string(encoded), true, nil)
	}
}

func (b *PromptBuilder) AddDriverState(source string, state json.RawMessage) {
	if len(state) > 0 {
		b.addTextBlock("driver-state", CategoryDriverState, source, string(state), false, nil)
	}
}

func (b *PromptBuilder) SetOutputReserve(tokens int) {
	if tokens > 0 {
		b.result.Blocks = append(b.result.Blocks, Block{ID: "output-reserve", Category: CategoryOutputReserve, Source: "runtime", Tokens: tokens, Exact: false})
	}
}

func (b *PromptBuilder) Result() PromptResult {
	result := b.result
	result.Turns = append([]protocol.Turn(nil), result.Turns...)
	result.Tools = append([]protocol.ToolDefinition(nil), result.Tools...)
	result.Blocks = append([]Block(nil), result.Blocks...)
	return result
}

func (b *PromptBuilder) addTextBlock(id string, category Category, source, text string, protected bool, metadata map[string]any) {
	b.result.Blocks = append(b.result.Blocks, Block{
		ID: id, Category: category, Source: source, Bytes: len([]byte(text)), Tokens: b.estimator.Estimate(text), Exact: false, Protected: protected, Metadata: metadata,
	})
}

func toolResultCategory(result *protocol.ToolResult) (Category, string) {
	if result.Metadata != nil {
		if server, ok := result.Metadata["mcp_server"].(string); ok && server != "" {
			return CategoryMCPToolResult, server
		}
		if name, ok := result.Metadata["skill_name"].(string); ok && name != "" {
			if resource, ok := result.Metadata["resource"].(string); ok && resource != "" {
				return CategorySkillResource, name + ":" + resource
			}
		}
	}
	return CategoryBuiltinToolResult, result.CallID
}

func PaginateSkillCatalog(catalog string, maxPayloadBytes int) []string {
	trimmed := strings.TrimSpace(catalog)
	if trimmed == "" {
		return nil
	}
	if maxPayloadBytes <= 0 {
		maxPayloadBytes = 8 << 10
	}
	lines := strings.Split(trimmed, "\n")
	items := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "<skill>") {
			items = append(items, line)
		}
	}
	if len(items) == 0 {
		items = []string{trimmed}
	}
	payloads := make([][]string, 1)
	for _, item := range items {
		current := payloads[len(payloads)-1]
		used := len(strings.Join(current, "\n"))
		if len(current) > 0 && used+1+len(item) > maxPayloadBytes {
			payloads = append(payloads, nil)
		}
		payloads[len(payloads)-1] = append(payloads[len(payloads)-1], item)
	}
	pages := make([]string, 0, len(payloads))
	for index, payload := range payloads {
		pages = append(pages, fmt.Sprintf("<available_skills page=\"%d\" pages=\"%d\">\n  %s\n</available_skills>", index+1, len(payloads), strings.Join(payload, "\n  ")))
	}
	return pages
}
