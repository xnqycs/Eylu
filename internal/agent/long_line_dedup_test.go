package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextledger "Eylu/internal/context"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

// A11, fixed: a very long single line whose body does not fit the byte budget is cut
// *inside* the line. No line range can describe a partial line, so for a long time
// such a body was unusable as a canonical and a repeated read of it earned nothing.
// The bytes that survived are now reported instead, and an identical repeated read is
// replaced by a reference to the body that already holds them.
//
// The first attempt at measuring this reported that the premise did not reproduce,
// and the measurement was wrong: it read the file and fed the result straight to the
// prompt builder, skipping the trimming step that produces the shape. This case goes
// through contextualizeTurn, which is where the shape is made.
func TestVeryLongSingleLineEarnsTheBenefitAfterTrimming(t *testing.T) {
	workspace := t.TempDir()
	// One line, far larger than the budget below, with no newline to split on.
	line := strings.Repeat("x", 4000)
	writeLongLine(t, workspace, "long.txt", line)

	reader, err := tool.NewReadFile(workspace, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(map[string]any{"path": "long.txt"})
	if err != nil {
		t.Fatal(err)
	}
	read := reader.Execute(context.Background(), arguments)
	if read.IsError {
		t.Fatalf("read failed: %s", read.Content)
	}

	// The context budget the window would give one tool result. Every whole line is
	// larger than it, so nothing but a byte cut is possible.
	const budget = 512
	trimmedTurn, changed := contextualizeTurn(protocol.Turn{
		ID: "turn-1", Role: protocol.RoleTool,
		Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: &read}},
	}, budget)
	if !changed {
		t.Fatal("the fixture was not trimmed, so it does not model the shape A11 describes")
	}
	trimmed := trimmedTurn.Parts[0].ToolResult
	if trimmed.Metadata["context_truncated"] != true {
		t.Fatalf("the trimmed result was not marked: %#v", trimmed.Metadata)
	}
	if !trimmed.Truncated {
		t.Fatal("the trimmed result does not say it was truncated")
	}
	// No line range is reported, because a partial line is not a whole one and must
	// never be read as one.
	if _, reported := trimmed.Metadata["retained_ranges"]; reported {
		t.Fatalf("a partial line was reported as a line range: %#v", trimmed.Metadata["retained_ranges"])
	}
	// The bytes that survived are reported instead, and they are spans of the one
	// line that was cut.
	spans, ok := trimmed.Metadata["retained_bytes"].([]protocol.ByteRange)
	if !ok || len(spans) != 2 {
		t.Fatalf("retained bytes = %#v", trimmed.Metadata["retained_bytes"])
	}
	for _, span := range spans {
		if span.Start >= span.End || span.End > len(line) {
			t.Fatalf("span %#v is not a real range of the %d-byte line", span, len(line))
		}
	}

	// Two reads of the same region, both trimmed the same way, through the real
	// prompt builder: the second is replaced by a reference, because the first body
	// really does hold every byte of it.
	builder := contextledger.NewPromptBuilder(contextledger.ApproxEstimator{BytesPerToken: 1})
	builder.AddTurn(trimmedTurn)
	builder.AddTurn(trimmedTurn)
	result := builder.Result()
	if result.SliceStats.Deduplicated != 1 {
		t.Fatalf("the trimmed long line still earns nothing: %#v", result.SliceStats)
	}
	second := result.Turns[1].Parts[0].ToolResult.Content
	if !strings.Contains(second, "code slice reference") {
		t.Fatalf("the second body was not replaced: %q", second)
	}
	// The reference stands for part of a line and says so, so the model is not told
	// it has the whole line.
	if !strings.Contains(second, "bytes=") {
		t.Fatalf("the reference does not name the bytes it stands for: %q", second)
	}
	estimator := contextledger.ApproxEstimator{BytesPerToken: 1}
	before, after := estimator.Estimate(trimmed.Content), estimator.Estimate(second)
	t.Logf("long line %d bytes: first read %d tokens, repeated read %d tokens with the fix (was %d, saved %d)",
		len(line), before, after, before, before-after)
	if after >= before {
		t.Fatalf("the repeated read still costs %d tokens, no better than %d", after, before)
	}
}

// A body kept whole and larger than the reference still earns the benefit through
// the line-range path, so the byte-span work added a case rather than replacing one.
func TestAWholeBodyLargerThanAReferenceStillEarnsTheBenefit(t *testing.T) {
	workspace := t.TempDir()
	// Comfortably larger than the reference that will stand for it.
	writeLongLine(t, workspace, "short.txt", strings.Repeat("a whole body line\n", 20))
	reader, err := tool.NewReadFile(workspace, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(map[string]any{"path": "short.txt"})
	if err != nil {
		t.Fatal(err)
	}
	read := reader.Execute(context.Background(), arguments)
	turn, _ := contextualizeTurn(protocol.Turn{
		ID: "turn-1", Role: protocol.RoleTool,
		Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: &read}},
	}, 4096)
	if turn.Parts[0].ToolResult.Truncated {
		t.Fatal("a short body was trimmed")
	}
	builder := contextledger.NewPromptBuilder(contextledger.ApproxEstimator{BytesPerToken: 1})
	builder.AddTurn(turn)
	builder.AddTurn(turn)
	if result := builder.Result(); result.SliceStats.Deduplicated != 1 {
		t.Fatalf("a whole body did not deduplicate: %#v", result.SliceStats)
	}
}

func writeLongLine(t *testing.T, workspace, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A body smaller than the reference that would stand for it keeps its content: the
// reference is very nearly a fixed size, so below that size replacing it grows the
// request instead of shrinking it. This goes through the real read tool so the size
// being compared is the one production produces.
func TestASmallBodyKeepsItsContent(t *testing.T) {
	workspace := t.TempDir()
	writeLongLine(t, workspace, "tiny.txt", "one short line\n")
	reader, err := tool.NewReadFile(workspace, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(map[string]any{"path": "tiny.txt"})
	if err != nil {
		t.Fatal(err)
	}
	builder := contextledger.NewPromptBuilder(contextledger.ApproxEstimator{BytesPerToken: 1})
	turns := make([]protocol.Turn, 0, 2)
	for attempt := 0; attempt < 2; attempt++ {
		read := reader.Execute(context.Background(), arguments)
		if read.IsError {
			t.Fatalf("read failed: %s", read.Content)
		}
		turn, _ := contextualizeTurn(protocol.Turn{
			ID: "turn", Role: protocol.RoleTool,
			Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: &read}},
		}, 4096)
		turns = append(turns, turn)
		builder.AddTurn(turn)
	}
	result := builder.Result()
	if result.SliceStats.Deduplicated != 0 || result.SliceStats.NotWorthReplacing != 1 {
		t.Fatalf("stats = %#v", result.SliceStats)
	}
	body := turns[0].Parts[0].ToolResult.Content
	if got := result.Turns[1].Parts[0].ToolResult.Content; got != protocol.FrameUntrusted(body) {
		t.Fatalf("the small body was replaced anyway: %q", got)
	}
	// The decision is visible on the block rather than being an absence.
	if skipped, _ := result.Blocks[1].Metadata["deduplication_not_worth_it"].(bool); !skipped {
		t.Fatalf("the block does not record why it kept its body: %#v", result.Blocks[1].Metadata)
	}
}
