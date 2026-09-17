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
	// retained lists the line ranges whose bodies are really present in the
	// request. An empty list means the whole declared range is present.
	retained []protocol.LineRange
	// retainedBytes holds the byte spans of a single line for a body that could not
	// be kept whole by lines. It is mutually exclusive with retained: a body is
	// described by whole lines or by byte spans, never by both.
	retainedBytes []protocol.ByteRange
	turnIndex     int
	partIndex     int
	blockIndex    int
}

// covers reports whether a slice's real body contains one line range. Only a
// single retained range can cover a range completely: a range that spans an
// omitted middle is not covered, however many pieces survived.
func covers(declaredStart, declaredEnd int, retained []protocol.LineRange, start, end int) bool {
	if len(retained) == 0 {
		return declaredStart <= start && declaredEnd >= end
	}
	for _, item := range retained {
		if item.Start <= start && item.End >= end {
			return true
		}
	}
	return false
}

// coversCurrent reports whether a canonical slice's real body contains
// everything the current slice declares *and holds*.
//
// The two kinds of evidence are kept apart on purpose. A body held as byte spans
// is a fragment of one line: it covers nothing by line, and the only thing it can
// cover is another fragment of that same line whose bytes it contains. Reading a
// partial line as if it were a whole one is the mistake this rule exists to
// prevent - it would replace a full read with a reference to a body that does not
// contain it.
func (c canonicalSlice) coversCurrent(current codeSliceMetadata) bool {
	if len(c.retainedBytes) > 0 {
		return current.partialLine() && current.startLine == current.endLine &&
			byteSpansCover(c.retainedBytes, current.retainedBytes)
	}
	if current.partialLine() {
		// A fragment of a line is covered by a canonical that holds that whole
		// line - and only then, because it holds strictly less than the line.
		if current.startLine != current.endLine {
			return false
		}
		return covers(c.startLine, c.endLine, c.retained, current.startLine, current.endLine)
	}
	return covers(c.startLine, c.endLine, c.retained, current.startLine, current.endLine)
}

// partialLine reports whether a slice is a body cut inside a single line, which is
// the shape no whole-line rule can describe.
func (s codeSliceMetadata) partialLine() bool {
	return !s.complete && len(s.retained) == 0 && len(s.retainedBytes) > 0
}

// byteSpansCover reports whether the canonical holds every byte the current slice
// holds. A span that no canonical span contains makes the answer no: the canonical
// is missing bytes the reference would have to stand for.
func byteSpansCover(canonical, current []protocol.ByteRange) bool {
	if len(current) == 0 {
		return false
	}
	for _, want := range current {
		covered := false
		for _, span := range canonical {
			if span.Line == want.Line && span.Start <= want.Start && span.End >= want.End {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
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
			codeSlice, isSlice := parseCodeSlice(part.ToolResult.Metadata)
			if isSlice {
				// A body that was trimmed for the context window no longer
				// covers the declared range, so it is not a reliable canonical
				// code slice.
				codeSlice.complete = codeSlice.complete && !part.ToolResult.Truncated
			}
			if isSlice && !part.ToolResult.IsError {
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
			if isSlice && !part.ToolResult.IsError {
				canonicalID, _ := metadata["canonical_artifact_id"].(string)
				switch {
				case canonicalID != "":
					b.references = append(b.references, sliceReferenceRecord{
						canonicalID: canonicalID, path: codeSlice.path, fileHash: codeSlice.fileHash,
						startLine: codeSlice.startLine, endLine: codeSlice.endLine,
						turnIndex: turnIndex, partIndex: index, blockIndex: len(b.result.Blocks) - 1,
					})
				case codeSlice.complete || len(codeSlice.retained) > 0 || len(codeSlice.retainedBytes) > 0:
					// A fragment is canonical for the lines it really holds: the
					// whole declared range when it is complete, or only its
					// retained ranges when it was trimmed.
					b.slices = append(b.slices, canonicalSlice{
						path: codeSlice.path, fileHash: codeSlice.fileHash, artifactID: codeSlice.artifactID,
						startLine: codeSlice.startLine, endLine: codeSlice.endLine, retained: codeSlice.retained,
						retainedBytes: codeSlice.retainedBytes,
						turnIndex:     turnIndex, partIndex: index, blockIndex: len(b.result.Blocks) - 1,
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
	// complete reports whether the body covers the whole declared line range.
	complete bool
	// retained lists the line ranges whose bodies really survived a trim, so a
	// trimmed fragment can still be canonical for the lines it kept.
	retained []protocol.LineRange
	// retainedBytes lists the byte spans of one line that survived when no whole
	// line could be kept.
	retainedBytes []protocol.ByteRange
}

func (s codeSliceMetadata) metadata() map[string]any {
	metadata := map[string]any{
		"path": s.path, "file_hash": s.fileHash, "artifact_id": s.artifactID,
		"start_line": s.startLine, "end_line": s.endLine, "cache_hit": s.cacheHit,
		"slice_complete": s.complete,
	}
	if len(s.retained) > 0 {
		metadata["retained_ranges"] = s.retained
	}
	if len(s.retainedBytes) > 0 {
		metadata["retained_bytes"] = s.retainedBytes
	}
	return metadata
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
	complete := true
	// The tool declares whether it returned the whole requested range, and the
	// context layer declares whether it later trimmed the body.
	if value, ok := metadata["lines_complete"].(bool); ok && !value {
		complete = false
	}
	if value, ok := metadata["context_truncated"].(bool); ok && value {
		complete = false
	}
	return codeSliceMetadata{
		path: path, fileHash: fileHash, artifactID: artifactID, startLine: startLine, endLine: endLine,
		cacheHit: cacheHit, complete: complete, retained: parseRetainedRanges(metadata["retained_ranges"]),
		retainedBytes: parseRetainedBytes(metadata["retained_bytes"]),
	}, true
}

// parseRetainedRanges reads the retained line ranges of a trimmed fragment. The
// value survives a JSON round trip through the session store, so both the typed
// and the decoded forms are accepted.
func parseRetainedRanges(value any) []protocol.LineRange {
	switch typed := value.(type) {
	case []protocol.LineRange:
		return append([]protocol.LineRange(nil), typed...)
	case []any:
		ranges := make([]protocol.LineRange, 0, len(typed))
		for _, item := range typed {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			start, startOK := promptInt(entry["start"])
			end, endOK := promptInt(entry["end"])
			if !startOK || !endOK || start <= 0 || end < start {
				continue
			}
			ranges = append(ranges, protocol.LineRange{Start: start, End: end})
		}
		return ranges
	default:
		return nil
	}
}

// parseRetainedBytes reads the byte spans of a body that could not be kept by
// lines. Like the retained ranges it survives a JSON round trip through the
// session store, so both the typed and the decoded forms are accepted.
func parseRetainedBytes(value any) []protocol.ByteRange {
	switch typed := value.(type) {
	case []protocol.ByteRange:
		return append([]protocol.ByteRange(nil), typed...)
	case []any:
		spans := make([]protocol.ByteRange, 0, len(typed))
		for _, item := range typed {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			line, lineOK := promptInt(entry["line"])
			start, startOK := promptInt(entry["start"])
			end, endOK := promptInt(entry["end"])
			if !lineOK || !startOK || !endOK || line <= 0 || start < 0 || end <= start {
				continue
			}
			spans = append(spans, protocol.ByteRange{Line: line, Start: start, End: end})
		}
		return spans
	default:
		return nil
	}
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
		// A canonical fragment is only usable for a range whose body it really
		// holds: a trimmed fragment covers the lines it kept, and a range that
		// spans its omitted middle is not covered at all.
		if existing.path == current.path && existing.fileHash == current.fileHash && existing.coversCurrent(current) {
			copy := existing
			containing = &copy
		}
	}
	if stale {
		b.result.SliceStats.Stale++
	}
	if containing != nil {
		b.result.SliceStats.Deduplicated++
		return sliceReference(containing.artifactID, current.path, current.startLine, current.endLine, current.fileHash, containing.retainedBytes), containing.artifactID, stale
	}
	// A fragment becomes canonical for the lines it actually holds. A complete
	// fragment holds its whole declared range; a trimmed one holds only its
	// retained ranges, which the containment check above enforces.
	if !current.complete && len(current.retained) == 0 && len(current.retainedBytes) == 0 {
		return content, "", stale
	}
	// A fragment of a line never supersedes another canonical: it holds no whole
	// line, so it cannot be the better authority for any line range. It is still
	// registered below, so an identical later read can be replaced by it.
	if current.partialLine() {
		return content, "", stale
	}
	kept := b.slices[:0]
	for _, existing := range b.slices {
		// A fragment only supersedes a smaller one when it really holds that
		// fragment's whole range, so a trimmed fragment cannot replace a canonical
		// whose body it does not contain.
		if existing.path == current.path && existing.fileHash == current.fileHash &&
			current.startLine <= existing.startLine && current.endLine >= existing.endLine &&
			covers(current.startLine, current.endLine, current.retained, existing.startLine, existing.endLine) {
			for index := range b.references {
				reference := &b.references[index]
				if reference.canonicalID != existing.artifactID {
					continue
				}
				reference.canonicalID = current.artifactID
				updated := sliceReference(current.artifactID, reference.path, reference.startLine, reference.endLine, reference.fileHash, nil)
				b.result.Turns[reference.turnIndex].Parts[reference.partIndex].ToolResult.Content = updated
				block := &b.result.Blocks[reference.blockIndex]
				block.Bytes = len([]byte(updated))
				block.Tokens = b.estimator.Estimate(updated)
				block.Metadata["canonical_artifact_id"] = current.artifactID
			}
			reference := sliceReference(current.artifactID, existing.path, existing.startLine, existing.endLine, existing.fileHash, nil)
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

func sliceReference(artifactID, path string, startLine, endLine int, fileHash string, spans []protocol.ByteRange) string {
	// A reference that stands for part of a line says so: the model must not read it
	// as the whole line, because the body it replaces was a fragment too.
	if len(spans) > 0 {
		return fmt.Sprintf("[code slice reference: artifact_id=%s path=%s lines=%d-%d bytes=%s file_hash=%s]",
			artifactID, path, startLine, endLine, formatByteSpans(spans), fileHash)
	}
	return fmt.Sprintf("[code slice reference: artifact_id=%s path=%s lines=%d-%d file_hash=%s]", artifactID, path, startLine, endLine, fileHash)
}

func formatByteSpans(spans []protocol.ByteRange) string {
	parts := make([]string, 0, len(spans))
	for _, span := range spans {
		parts = append(parts, fmt.Sprintf("%d:%d-%d", span.Line, span.Start, span.End))
	}
	return strings.Join(parts, ",")
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
