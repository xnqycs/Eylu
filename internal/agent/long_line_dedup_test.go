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

// A11, reproduced: a very long single line whose body does not fit the byte budget
// is cut *inside* the line, and a partially kept line cannot back a range, so no
// retained range is reported. The result is therefore neither a complete slice nor
// a fragment canonical, and a repeated read of the same region earns nothing.
//
// The first attempt at measuring this reported that the premise did not reproduce,
// and the measurement was wrong: it read the file and fed the result straight to the
// prompt builder, skipping the trimming step that produces the shape. The case below
// goes through contextualizeTurn, which is where the shape is made.
func TestVeryLongSingleLineEarnsNoDeduplicationAfterTrimming(t *testing.T) {
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
	if _, reported := trimmed.Metadata["retained_ranges"]; reported {
		// If this ever starts reporting an in-line range, the gap is closed and this
		// case must be rewritten to assert the benefit instead.
		t.Fatalf("an in-line retained range is now reported: %#v", trimmed.Metadata["retained_ranges"])
	}
	if !trimmed.Truncated {
		t.Fatal("the trimmed result does not say it was truncated")
	}

	// Two reads of the same region, both trimmed the same way, through the real
	// prompt builder: the second must not be replaced by a reference, because no
	// range backs the replacement.
	builder := contextledger.NewPromptBuilder(contextledger.ApproxEstimator{BytesPerToken: 1})
	builder.AddTurn(trimmedTurn)
	builder.AddTurn(trimmedTurn)
	result := builder.Result()
	if result.SliceStats.Deduplicated != 0 {
		t.Fatalf("the trimmed long line deduplicated after all: %#v", result.SliceStats)
	}
	second := result.Turns[1].Parts[0].ToolResult.Content
	if second != trimmed.Content {
		t.Fatalf("the second body changed: %q", second)
	}
	// The saving that is therefore unavailable, which is what the fix would buy.
	estimator := contextledger.ApproxEstimator{BytesPerToken: 1}
	t.Logf("long line %d bytes -> trimmed to %d bytes; a repeated read keeps paying %d tokens because no range is reported",
		len(line), len(trimmed.Content), estimator.Estimate(trimmed.Content))
	if estimator.Estimate(trimmed.Content) <= 0 {
		t.Fatal("the trimmed body is empty, so this case proves nothing")
	}
}

// The same body, small enough to keep whole, does earn the benefit - so the loss
// above is about the shape and not about deduplication being broken in general.
func TestAShortBodyStillEarnsTheBenefit(t *testing.T) {
	workspace := t.TempDir()
	writeLongLine(t, workspace, "short.txt", "a short body\nwith two lines\n")
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
