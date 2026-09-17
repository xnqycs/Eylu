package context

import (
	"strings"
	"testing"

	"Eylu/internal/protocol"
)

// partialSliceTurn records a code slice result whose body was cut inside one line:
// it reports the byte spans it really holds and no line range at all.
func partialSliceTurn(builder *PromptBuilder, id, artifact, hash, content string, line int, spans []protocol.ByteRange) *protocol.ToolResult {
	metadata := map[string]any{
		"relative_path": "main.go", "file_hash": hash, "artifact_id": artifact,
		"start_line": line, "end_line": line, "context_truncated": true,
	}
	if len(spans) > 0 {
		metadata["retained_bytes"] = spans
	}
	result := &protocol.ToolResult{CallID: id, Content: content, Truncated: true, Metadata: metadata}
	builder.AddTurn(protocol.Turn{ID: id, Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: result}}})
	return result
}

func byteSpans(pairs ...int) []protocol.ByteRange {
	spans := make([]protocol.ByteRange, 0, len(pairs)/3)
	for index := 0; index+2 < len(pairs); index += 3 {
		spans = append(spans, protocol.ByteRange{Line: pairs[index], Start: pairs[index+1], End: pairs[index+2]})
	}
	return spans
}

// The benefit A11 was about: a body that cannot be kept whole by lines reports the
// bytes it holds, so an identical repeated read is replaced by a reference instead
// of being paid for twice.
func TestFragmentOfOneLineBecomesCanonicalForItsOwnBytes(t *testing.T) {
	builder := NewPromptBuilder(ApproxEstimator{BytesPerToken: 1})
	spans := byteSpans(1, 0, 300, 1, 900, 1000)
	partialSliceTurn(builder, "first", "first", "hash", "head ... tail", 1, spans)
	second := partialSliceTurn(builder, "second", "second", "hash", "head ... tail", 1, spans)

	result := builder.Result()
	if result.SliceStats.Deduplicated != 1 {
		t.Fatalf("stats = %#v", result.SliceStats)
	}
	// The builder copies content into the result rather than mutating the input, so
	// the replacement has to be read from what it built.
	built := result.Turns[1].Parts[0].ToolResult.Content
	if second.Content != "head ... tail" {
		t.Fatalf("the input was mutated: %q", second.Content)
	}
	// The reference must say it stands for part of a line, or the model would read
	// it as the whole line.
	if !strings.Contains(built, "bytes=") {
		t.Fatalf("the reference does not name the bytes it stands for: %q", built)
	}
	if !strings.Contains(built, "lines=1-1") {
		t.Fatalf("the reference lost the line it belongs to: %q", built)
	}
}

// A fragment of a line must never be read as a whole one. These are the cases that
// decide whether the rule is safe, and each one has to keep the body.
func TestFragmentOfOneLineNeverCoversMoreThanItHolds(t *testing.T) {
	cases := []struct {
		name string
		// current is the later slice, built after the canonical fragment below.
		current       func(builder *PromptBuilder) *protocol.ToolResult
		wantReference bool
		why           string
	}{
		{
			name: "a whole line read later is not covered by a fragment of it",
			current: func(builder *PromptBuilder) *protocol.ToolResult {
				return addSliceTurn(builder, "whole", "whole", "hash", "the entire line", 1, 1)
			},
			wantReference: false,
			why:           "the canonical holds part of the line, so it cannot back a reference for all of it",
		},
		{
			name: "a later fragment with wider spans is not covered",
			current: func(builder *PromptBuilder) *protocol.ToolResult {
				return partialSliceTurn(builder, "wider", "wider", "hash", "more head ... more tail", 1,
					byteSpans(1, 0, 500, 1, 800, 1000))
			},
			wantReference: false,
			why:           "the canonical is missing the bytes the wider fragment holds",
		},
		{
			name: "a later fragment of another line is not covered",
			current: func(builder *PromptBuilder) *protocol.ToolResult {
				return partialSliceTurn(builder, "other", "other", "hash", "another line", 2,
					byteSpans(2, 0, 300, 2, 900, 1000))
			},
			wantReference: false,
			why:           "byte spans belong to one line and say nothing about another",
		},
		{
			name: "a later fragment of a different revision is not covered",
			current: func(builder *PromptBuilder) *protocol.ToolResult {
				return partialSliceTurn(builder, "stale", "stale", "other-hash", "head ... tail", 1,
					byteSpans(1, 0, 300, 1, 900, 1000))
			},
			wantReference: false,
			why:           "the bytes of another revision are different bytes",
		},
		{
			name: "a later fragment inside the canonical spans is covered",
			current: func(builder *PromptBuilder) *protocol.ToolResult {
				return partialSliceTurn(builder, "narrower", "narrower", "hash", "less head ... less tail", 1,
					byteSpans(1, 0, 100, 1, 950, 1000))
			},
			wantReference: true,
			why:           "the canonical holds every byte the narrower fragment holds",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			builder := NewPromptBuilder(ApproxEstimator{BytesPerToken: 1})
			partialSliceTurn(builder, "canonical", "canonical", "hash", "head ... tail", 1,
				byteSpans(1, 0, 300, 1, 900, 1000))
			current := testCase.current(builder)
			result := builder.Result()
			// The property that matters is about the *later* slice: whether its body
			// survived or was replaced by a reference to a body that holds it. The
			// total deduplication count is deliberately not the assertion, because the
			// earlier fragment may itself become a reference to the later body - a
			// forward reference, which this design already makes elsewhere and which
			// loses nothing, since the body it points at is in the same request.
			later := result.Turns[len(result.Turns)-1].Parts[0].ToolResult.Content
			replaced := strings.Contains(later, "code slice reference")
			if replaced != testCase.wantReference {
				t.Fatalf("the later body was replaced=%t, want %t: %s (content = %q)",
					replaced, testCase.wantReference, testCase.why, later)
			}
			if !testCase.wantReference && later != current.Content {
				t.Fatalf("the later body was changed: %q, want %q", later, current.Content)
			}
			if testCase.wantReference && later == "" {
				t.Fatal("the later slice lost its content entirely")
			}
		})
	}
}

// A fragment of a line must not become the authority for a line that a later,
// complete read holds: the complete body is what the references should point at.
func TestAFragmentOfALineDoesNotSupersedeAWholeLineCanonical(t *testing.T) {
	builder := NewPromptBuilder(ApproxEstimator{BytesPerToken: 1})
	whole := addSliceTurn(builder, "whole", "whole", "hash", "the entire line", 1, 1)
	partialSliceTurn(builder, "partial", "partial", "hash", "head ... tail", 1, byteSpans(1, 0, 300, 1, 900, 1000))

	result := builder.Result()
	if result.Turns[0].Parts[0].ToolResult.Content != "the entire line" {
		t.Fatalf("the whole-line canonical was superseded: %q", result.Turns[0].Parts[0].ToolResult.Content)
	}
	if whole.Content != "the entire line" {
		t.Fatalf("the input was mutated: %q", whole.Content)
	}
	// The later fragment is not covered by the whole line either: the whole line
	// read kept lines 1-1, and the fragment is a slice of that same line, so it is
	// covered - the canonical really does hold every byte of it.
	if result.SliceStats.Deduplicated != 1 {
		t.Fatalf("stats = %#v", result.SliceStats)
	}
	_ = whole
}

// The byte spans survive the JSON round trip the session store puts metadata
// through, and a decoded one still cannot be read as a whole line.
func TestDecodedByteSpansKeepTheirMeaning(t *testing.T) {
	decoded := parseRetainedBytes([]any{
		map[string]any{"line": float64(1), "start": float64(0), "end": float64(300)},
		map[string]any{"line": float64(1), "start": float64(900), "end": float64(1000)},
		map[string]any{"line": float64(0), "start": float64(0), "end": float64(10)},
		map[string]any{"line": float64(2), "start": float64(5), "end": float64(5)},
	})
	if len(decoded) != 2 {
		t.Fatalf("decoded = %#v", decoded)
	}
	if !byteSpansCover(decoded, byteSpans(1, 10, 20)) {
		t.Fatal("a span inside the decoded ranges was not covered")
	}
	if byteSpansCover(decoded, byteSpans(1, 10, 400)) {
		t.Fatal("a span reaching past the decoded ranges was covered")
	}
	if byteSpansCover(decoded, byteSpans(3, 0, 10)) {
		t.Fatal("a span on another line was covered")
	}
}
