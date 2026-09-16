package context

import (
	"strings"
	"testing"

	"Eylu/internal/protocol"
)

// addSliceTurn records one code slice result on the builder.
func addSliceTurn(builder *PromptBuilder, id, artifact, hash, content string, start, end int) *protocol.ToolResult {
	result := &protocol.ToolResult{CallID: id, Content: content, Metadata: map[string]any{
		"relative_path": "main.go", "file_hash": hash, "artifact_id": artifact,
		"start_line": start, "end_line": end,
	}}
	builder.AddTurn(protocol.Turn{ID: id, Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: result}}})
	return result
}

// A fragment whose body was trimmed for the context window must not become the
// canonical slice for the range it claims, or a later read of the omitted middle
// would be replaced by a reference to a body that does not contain it.
func TestPromptBuilderKeepsBodyOfSliceReadAfterTrimmedFragment(t *testing.T) {
	builder := NewPromptBuilder(ApproxEstimator{BytesPerToken: 1})
	big := &protocol.ToolResult{CallID: "big", Content: "trimmed body", Truncated: true, Metadata: map[string]any{
		"relative_path": "main.go", "file_hash": "hash", "artifact_id": "big",
		"start_line": 1, "end_line": 1000, "lines_complete": false, "context_truncated": true,
	}}
	builder.AddTurn(protocol.Turn{ID: "big", Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: big}}})
	middle := addSliceTurn(builder, "middle", "middle", "hash", "the omitted middle lines", 400, 450)

	result := builder.Result()
	got := result.Turns[1].Parts[0].ToolResult.Content
	if got != "the omitted middle lines" {
		t.Fatalf("middle slice was replaced by a reference: %q", got)
	}
	if strings.Contains(got, "code slice reference") {
		t.Fatalf("middle slice content = %q", got)
	}
	if middle.Content != "the omitted middle lines" {
		t.Fatalf("input was mutated: %q", middle.Content)
	}
	if result.SliceStats.Deduplicated != 0 {
		t.Fatalf("stats = %#v", result.SliceStats)
	}
}

// A trimmed fragment must not supersede a complete smaller slice that was read
// earlier, and must not redirect the references that point at it.
func TestPromptBuilderTrimmedFragmentDoesNotSupersedeCompleteSlice(t *testing.T) {
	builder := NewPromptBuilder(ApproxEstimator{BytesPerToken: 1})
	small := addSliceTurn(builder, "small", "small", "hash", "exact small body", 400, 450)
	big := &protocol.ToolResult{CallID: "big", Content: "trimmed body", Truncated: true, Metadata: map[string]any{
		"relative_path": "main.go", "file_hash": "hash", "artifact_id": "big",
		"start_line": 1, "end_line": 1000, "context_truncated": true,
	}}
	builder.AddTurn(protocol.Turn{ID: "big", Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: big}}})

	result := builder.Result()
	if result.Turns[0].Parts[0].ToolResult.Content != "exact small body" {
		t.Fatalf("complete slice was superseded: %q", result.Turns[0].Parts[0].ToolResult.Content)
	}
	if small.Content != "exact small body" || big.Content != "trimmed body" {
		t.Fatalf("inputs were mutated: %q / %q", small.Content, big.Content)
	}
	if result.SliceStats.Deduplicated != 0 {
		t.Fatalf("stats = %#v", result.SliceStats)
	}
}

// Complete fragments still deduplicate and still supersede each other.
func TestPromptBuilderStillDeduplicatesCompleteSlices(t *testing.T) {
	builder := NewPromptBuilder(ApproxEstimator{BytesPerToken: 1})
	addSliceTurn(builder, "first", "first", "hash", "lines 10 through 20", 10, 20)
	addSliceTurn(builder, "repeat", "repeat", "hash", "lines 10 through 20", 10, 20)

	result := builder.Result()
	if result.Turns[1].Parts[0].ToolResult.Content == "lines 10 through 20" {
		t.Fatal("a repeated complete read was not deduplicated")
	}
	if !strings.Contains(result.Turns[1].Parts[0].ToolResult.Content, "artifact_id=first") {
		t.Fatalf("repeat content = %q", result.Turns[1].Parts[0].ToolResult.Content)
	}
	if got := result.Blocks[1].Metadata["slice_complete"]; got != true {
		t.Fatalf("slice_complete = %#v", got)
	}
}

// A tool-declared incomplete range is not a reliable canonical either.
func TestPromptBuilderTreatsToolTruncatedRangeAsIncomplete(t *testing.T) {
	builder := NewPromptBuilder(ApproxEstimator{BytesPerToken: 1})
	partial := &protocol.ToolResult{CallID: "partial", Content: "first lines only", Metadata: map[string]any{
		"relative_path": "main.go", "file_hash": "hash", "artifact_id": "partial",
		"start_line": 1, "end_line": 1000, "lines_complete": false,
	}}
	builder.AddTurn(protocol.Turn{ID: "partial", Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: partial}}})
	addSliceTurn(builder, "middle", "middle", "hash", "middle body", 400, 450)

	result := builder.Result()
	if result.Turns[1].Parts[0].ToolResult.Content != "middle body" {
		t.Fatalf("middle slice = %q", result.Turns[1].Parts[0].ToolResult.Content)
	}
	if got := result.Blocks[0].Metadata["slice_complete"]; got != false {
		t.Fatalf("slice_complete = %#v", got)
	}
}

// A different file hash never satisfies a reference, complete or not.
func TestPromptBuilderNeverReferencesAnotherFileGeneration(t *testing.T) {
	builder := NewPromptBuilder(ApproxEstimator{BytesPerToken: 1})
	addSliceTurn(builder, "old", "old", "hash-one", "old generation body", 1, 100)
	addSliceTurn(builder, "new", "new", "hash-two", "new generation body", 10, 20)

	result := builder.Result()
	if got := result.Turns[1].Parts[0].ToolResult.Content; got != "new generation body" {
		t.Fatalf("new generation was replaced: %q", got)
	}
	if result.SliceStats.Stale != 1 {
		t.Fatalf("stats = %#v", result.SliceStats)
	}
}
