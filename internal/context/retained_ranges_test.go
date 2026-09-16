package context

import (
	"strings"
	"testing"

	"Eylu/internal/protocol"
)

// A trimmed fragment may still serve as a canonical slice for the lines it
// retained, which recovers part of the deduplication value a trim would otherwise
// give up.
func TestTrimmedFragmentDeduplicatesOnlyTheLinesItRetained(t *testing.T) {
	builder := NewPromptBuilder(ApproxEstimator{BytesPerToken: 1})
	head := strings.Repeat("head line\n", 20)
	omitted := strings.Repeat("omitted line\n", 60)
	body := head + omitted + "tail marker"
	trimmed := &protocol.ToolResult{CallID: "big", Content: body, Truncated: true, Metadata: map[string]any{
		"relative_path": "main.go", "file_hash": "hash", "artifact_id": "big",
		"start_line": 1, "end_line": 100, "context_truncated": true, "lines_complete": false,
		"retained_ranges": []protocol.LineRange{{Start: 1, End: 20}, {Start: 81, End: 100}},
	}}
	builder.AddTurn(protocol.Turn{ID: "big", Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: trimmed}}})

	// A read inside the retained head is covered, so a reference is valid.
	inside := addSliceTurn(builder, "inside", "inside", "hash", "lines 5 to 8", 5, 8)
	// A read over the omitted middle is not covered, so its body must stay.
	middle := addSliceTurn(builder, "middle", "middle", "hash", "the omitted middle", 40, 45)
	// A read over the retained tail is covered too.
	tail := addSliceTurn(builder, "tail", "tail", "hash", "the retained tail", 82, 84)

	result := builder.Result()
	if got := result.Turns[1].Parts[0].ToolResult.Content; !strings.Contains(got, "artifact_id=big") {
		t.Fatalf("a retained range was not deduplicated: %q", got)
	}
	if got := result.Turns[2].Parts[0].ToolResult.Content; got != "the omitted middle" {
		t.Fatalf("the omitted middle was replaced by a reference: %q", got)
	}
	if got := result.Turns[3].Parts[0].ToolResult.Content; !strings.Contains(got, "artifact_id=big") {
		t.Fatalf("the retained tail was not deduplicated: %q", got)
	}
	if result.SliceStats.Deduplicated != 2 {
		t.Fatalf("stats = %#v", result.SliceStats)
	}
	if inside.Content != "lines 5 to 8" || middle.Content != "the omitted middle" || tail.Content != "the retained tail" {
		t.Fatal("the input transcript was mutated")
	}
}

// A range that spans two retained pieces is not covered, because the omitted
// middle is missing from the canonical body.
func TestReferenceRequiresOneRetainedRange(t *testing.T) {
	builder := NewPromptBuilder(ApproxEstimator{BytesPerToken: 1})
	trimmed := &protocol.ToolResult{CallID: "big", Content: "head\ntail", Truncated: true, Metadata: map[string]any{
		"relative_path": "main.go", "file_hash": "hash", "artifact_id": "big",
		"start_line": 1, "end_line": 100, "context_truncated": true,
		"retained_ranges": []protocol.LineRange{{Start: 1, End: 10}, {Start: 91, End: 100}},
	}}
	builder.AddTurn(protocol.Turn{ID: "big", Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: trimmed}}})
	spanning := addSliceTurn(builder, "spanning", "spanning", "hash", "lines 5 to 95", 5, 95)

	result := builder.Result()
	if got := result.Turns[1].Parts[0].ToolResult.Content; got != "lines 5 to 95" {
		t.Fatalf("a range spanning the omitted middle was deduplicated: %q", got)
	}
	if spanning.Content != "lines 5 to 95" {
		t.Fatal("the input transcript was mutated")
	}
}

// A trimmed fragment does not supersede a complete canonical whose body it does
// not hold.
func TestTrimmedFragmentDoesNotSupersedeACompleteCanonical(t *testing.T) {
	builder := NewPromptBuilder(ApproxEstimator{BytesPerToken: 1})
	complete := addSliceTurn(builder, "complete", "complete", "hash", "the complete middle", 40, 50)
	trimmed := &protocol.ToolResult{CallID: "big", Content: "head\ntail", Truncated: true, Metadata: map[string]any{
		"relative_path": "main.go", "file_hash": "hash", "artifact_id": "big",
		"start_line": 1, "end_line": 100, "context_truncated": true,
		"retained_ranges": []protocol.LineRange{{Start: 1, End: 10}, {Start: 91, End: 100}},
	}}
	builder.AddTurn(protocol.Turn{ID: "big", Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: trimmed}}})

	result := builder.Result()
	if got := result.Turns[0].Parts[0].ToolResult.Content; got != "the complete middle" {
		t.Fatalf("the complete canonical was replaced: %q", got)
	}
	if complete.Content != "the complete middle" {
		t.Fatal("the input transcript was mutated")
	}
	// The trimmed fragment is still usable for the lines it kept.
	inside := addSliceTurn(builder, "inside", "inside", "hash", "lines 95 to 98", 95, 98)
	result = builder.Result()
	_ = inside
	if got := result.Turns[2].Parts[0].ToolResult.Content; !strings.Contains(got, "artifact_id=big") {
		t.Fatalf("the retained tail was not usable: %q", got)
	}
}

// The retained ranges survive a JSON round trip through the session store, so a
// rebuilt request applies the same coverage rule.
func TestRetainedRangesSurviveARebuild(t *testing.T) {
	builder := NewPromptBuilder(ApproxEstimator{BytesPerToken: 1})
	// The metadata is the decoded form a restored session produces.
	trimmed := &protocol.ToolResult{CallID: "big", Content: "head\ntail", Truncated: true, Metadata: map[string]any{
		"relative_path": "main.go", "file_hash": "hash", "artifact_id": "big",
		"start_line": 1, "end_line": 100, "context_truncated": true,
		"retained_ranges": []any{
			map[string]any{"start": float64(1), "end": float64(10)},
			map[string]any{"start": float64(91), "end": float64(100)},
		},
	}}
	builder.AddTurn(protocol.Turn{ID: "big", Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: trimmed}}})
	addSliceTurn(builder, "inside", "inside", "hash", "lines 3 to 6", 3, 6)
	addSliceTurn(builder, "middle", "middle", "hash", "lines 40 to 45", 40, 45)

	result := builder.Result()
	if got := result.Turns[1].Parts[0].ToolResult.Content; !strings.Contains(got, "artifact_id=big") {
		t.Fatalf("a decoded retained range was ignored: %q", got)
	}
	if got := result.Turns[2].Parts[0].ToolResult.Content; got != "lines 40 to 45" {
		t.Fatalf("a decoded omitted range was deduplicated: %q", got)
	}
}

// The token saving of deduplication is quantified, so the benefit of the
// mechanism is a measured number rather than an assumption.
func TestDeduplicationQuantifiesItsTokenSavings(t *testing.T) {
	estimator := ApproxEstimator{BytesPerToken: 4}
	region := strings.Repeat("func example() { return nil }\n", 40)
	const reads = 20
	builder := NewPromptBuilder(estimator)
	for read := 0; read < reads; read++ {
		result := &protocol.ToolResult{CallID: "read", Content: region, Metadata: map[string]any{
			"relative_path": "main.go", "file_hash": "hash", "artifact_id": "artifact-1",
			"start_line": 1, "end_line": 40,
		}}
		builder.AddTurn(protocol.Turn{ID: "turn", Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: result}}})
	}
	result := builder.Result()
	if result.SliceStats.Deduplicated != reads-1 {
		t.Fatalf("deduplicated = %d, want %d", result.SliceStats.Deduplicated, reads-1)
	}
	actual := 0
	for _, block := range result.Blocks {
		if block.Category == CategoryCodeSlice {
			actual += block.Tokens
		}
	}
	full := reads * estimator.Estimate(region)
	if actual >= full {
		t.Fatalf("deduplication saved nothing: actual=%d full=%d", actual, full)
	}
	saved := 100 - actual*100/full
	t.Logf("code slice tokens: %d instead of %d (%d%% saved over %d reads)", actual, full, saved, reads)
	if saved < 80 {
		t.Fatalf("deduplication saved only %d%%", saved)
	}
	// Content correctness is not traded for the saving: the first read keeps the
	// whole body.
	if !strings.Contains(result.Turns[0].Parts[0].ToolResult.Content, "func example()") {
		t.Fatal("the canonical body was dropped")
	}
}

// Deduplication must keep saving tokens on a conversation that reloads the same
// regions repeatedly; the numbers are the quantified benefit of the mechanism.
func BenchmarkCodeSliceDeduplication(b *testing.B) {
	region := strings.Repeat("func example() { return nil }\n", 40)
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		builder := NewPromptBuilder(ApproxEstimator{BytesPerToken: 4})
		for read := 0; read < 20; read++ {
			result := &protocol.ToolResult{CallID: "read", Content: region, Metadata: map[string]any{
				"relative_path": "main.go", "file_hash": "hash", "artifact_id": "artifact-1",
				"start_line": 1, "end_line": 40,
			}}
			builder.AddTurn(protocol.Turn{ID: "turn", Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: result}}})
		}
		result := builder.Result()
		if result.SliceStats.Deduplicated != 19 {
			b.Fatalf("deduplicated = %d, want 19", result.SliceStats.Deduplicated)
		}
	}
}
