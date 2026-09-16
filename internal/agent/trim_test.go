package agent

import (
	"strings"
	"testing"
	"unicode/utf8"

	"Eylu/internal/protocol"
)

// The trim keeps whole lines and reports exactly the file lines that survived, so
// a retained range is always backed by a body that is really present.
func TestTrimToolResultContentKeepsWholeLines(t *testing.T) {
	lines := make([]string, 100)
	for index := range lines {
		lines[index] = "line-" + string(rune('a'+index%26)) + strings.Repeat("x", 20)
	}
	body := strings.Join(lines, "\n")
	trimmed, retained, complete := trimToolResultContent(body, 600, 1)
	if complete {
		t.Fatal("a trimmed body reported itself as complete")
	}
	if len(retained) != 2 {
		t.Fatalf("retained = %#v", retained)
	}
	if !strings.Contains(trimmed, "summarized") {
		t.Fatalf("the omission marker is missing: %q", trimmed)
	}
	if len(trimmed) > 600 {
		t.Fatalf("trimmed length = %d", len(trimmed))
	}
	// Every retained range is fully present in the trimmed body: the lines it
	// names are the first and last lines of the retained pieces.
	first := retained[0]
	if first.Start != 1 {
		t.Fatalf("the head range does not start at the first line: %#v", first)
	}
	for _, item := range retained {
		if item.Start < 1 || item.End < item.Start || item.End > len(lines) {
			t.Fatalf("range out of bounds: %#v", item)
		}
		kept := strings.Join(lines[item.Start-1:item.End], "\n")
		if !strings.Contains(trimmed, kept) {
			t.Fatalf("the retained range %#v is not present in the trimmed body", item)
		}
	}
	last := retained[len(retained)-1]
	if last.End != len(lines) {
		t.Fatalf("the tail range does not reach the end of the body: %#v", last)
	}
}

// A complete body is returned untouched and reports no partial range.
func TestTrimToolResultContentLeavesShortBodiesAlone(t *testing.T) {
	for _, body := range []string{"", "one line", "a\nb\nc\n"} {
		trimmed, retained, complete := trimToolResultContent(body, 4096, 1)
		if !complete || trimmed != body || retained != nil {
			t.Fatalf("body %q: trimmed=%q retained=%#v complete=%t", body, trimmed, retained, complete)
		}
	}
}

// A very long single line cannot be kept whole, so it is cut inside the line and
// reports no retained range: a partial line may never back a reference.
func TestTrimToolResultContentFallsBackForALongLine(t *testing.T) {
	body := strings.Repeat("x", 4000) + "TAIL-MARKER"
	trimmed, retained, complete := trimToolResultContent(body, 400, 10)
	if complete || len(retained) != 0 {
		t.Fatalf("retained=%#v complete=%t", retained, complete)
	}
	if !strings.Contains(trimmed, "summarized") || !strings.Contains(trimmed, "TAIL-MARKER") || len(trimmed) > 400 {
		t.Fatalf("trimmed = %q", trimmed)
	}
}

// Multi-byte content stays valid UTF-8 and never splits a rune.
func TestTrimToolResultContentKeepsValidUTF8(t *testing.T) {
	body := strings.Repeat("中文内容行\n", 200)
	trimmed, _, complete := trimToolResultContent(body, 200, 1)
	if complete {
		t.Fatal("a trimmed body reported itself as complete")
	}
	if !utf8.ValidString(trimmed) {
		t.Fatal("trimmed content is not valid UTF-8")
	}
}

// The trimmed copy carries the retained ranges, and a read inside them may use a
// reference while a read over the omitted middle keeps its body.
func TestContextualizeTurnReportsRetainedRanges(t *testing.T) {
	lines := make([]string, 200)
	for index := range lines {
		lines[index] = "line content " + strings.Repeat("y", 30)
	}
	body := strings.Join(lines, "\n")
	original := sliceResult("big", "big", body, 1, 200)
	turn := protocol.Turn{ID: "tool", Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: original}}}
	contextTurn, keep := contextualizeTurn(turn, 800)
	if !keep {
		t.Fatal("the turn was dropped")
	}
	result := contextTurn.Parts[0].ToolResult
	if result.Metadata["context_truncated"] != true {
		t.Fatalf("metadata = %#v", result.Metadata)
	}
	retained, ok := result.Metadata["retained_ranges"].([]protocol.LineRange)
	if !ok || len(retained) == 0 {
		t.Fatalf("retained ranges = %#v", result.Metadata["retained_ranges"])
	}
	if retained[0].Start != 1 || retained[len(retained)-1].End != 200 {
		t.Fatalf("retained = %#v", retained)
	}
	if original.Content != body || original.Truncated {
		t.Fatal("the full transcript result was mutated")
	}
}
