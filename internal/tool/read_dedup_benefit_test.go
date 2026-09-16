package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextledger "Eylu/internal/context"
	"Eylu/internal/protocol"
)

// dedupShape is one file shape whose repeated read may or may not earn a
// deduplication benefit.
type dedupShape struct {
	name    string
	content string
	// wantDedup is what the shape must earn today.
	wantDedup bool
	// wantBenefit marks the shape whose token effect must be a win. Small bodies
	// deduplicate into a fixed-size reference that can cost more than the body it
	// replaces, which is a measured fact about the design, not a failure.
	wantBenefit bool
	// knownGap explains why a shape does not earn the benefit yet.
	knownGap string
}

func dedupShapes() []dedupShape {
	var longLine strings.Builder
	for index := 0; index < 40_000; index++ {
		longLine.WriteString("x")
	}
	return []dedupShape{
		{name: "empty file", content: "", wantDedup: false, knownGap: "there is no body to replace"},
		{name: "single line", content: "one line only\n", wantDedup: true},
		{name: "no trailing newline", content: "one line without a terminator", wantDedup: true},
		{name: "crlf and lf mixed", content: "first\r\nsecond\nthird\r\n", wantDedup: true},
		{name: "unicode multibyte", content: "第一行：中文与 emoji 🙂\n第二行：更多多字节字符\n", wantDedup: true},
		{
			// The plan's A11 premise is that a body which cannot be kept whole by
			// lines is trimmed by bytes with no reported line ranges, and therefore
			// earns nothing. With a real read and a byte limit that does not trim, the
			// long line deduplicates exactly like any other single-line file, so the
			// premise does NOT reproduce from this shape. Reproducing it needs a
			// result whose body was actually trimmed for the window
			// (context_truncated, no line ranges), which is what the hand-built
			// fixture in prompt_builder_slice_test.go models - and establishing that
			// the read path really emits that shape is the open question, not
			// something this baseline may assume.
			name: "one very long line", content: longLine.String() + "\n", wantDedup: true,
		},
	}
}

// The benefit of deduplication is measured on real reads of real files, per shape,
// because the interesting case is exactly the one the plan named: a body that
// cannot be kept whole. The numbers are reported for every shape; the shapes that
// must earn the benefit assert it, and the ones that do not say why.
func TestReadDeduplicationBenefitPerFileShape(t *testing.T) {
	for _, shape := range dedupShapes() {
		t.Run(shape.name, func(t *testing.T) {
			workspace := t.TempDir()
			path := filepath.Join(workspace, "shape.txt")
			writeShape(t, path, shape.content)

			reads := readTwice(t, workspace, "shape.txt")

			builder := contextledger.NewPromptBuilder(contextledger.ApproxEstimator{BytesPerToken: 1})
			for index, read := range reads {
				read.CallID = fmt.Sprintf("read-%d", index)
				builder.AddTurn(protocol.Turn{ID: read.CallID, Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: read}}})
			}
			result := builder.Result()
			deduplicated := result.SliceStats.Deduplicated
			// The builder copies content into the result rather than mutating the
			// input, so the benefit has to be read from what it built.
			firstBuilt := result.Turns[0].Parts[0].ToolResult.Content
			secondBuilt := result.Turns[1].Parts[0].ToolResult.Content
			estimator := contextledger.ApproxEstimator{BytesPerToken: 1}
			firstTokens := estimator.Estimate(firstBuilt)
			secondTokens := estimator.Estimate(secondBuilt)

			saved := firstTokens - secondTokens
			t.Logf("%s: body %d bytes, first read ~%d tokens, repeated read ~%d tokens, saved ~%d, deduplicated=%d",
				shape.name, len(shape.content), firstTokens, secondTokens, saved, deduplicated)

			if shape.wantDedup {
				if deduplicated == 0 {
					t.Fatalf("%s earned no deduplication benefit although it must: %#v", shape.name, result.SliceStats)
				}
				if shape.wantBenefit && saved <= 0 {
					t.Fatalf("%s saved %d tokens although it is the shape the benefit exists for", shape.name, saved)
				}
				if !shape.wantBenefit && saved > 0 {
					// Not a failure: a small body can deduplicate and still save
					// tokens. The opposite is what this baseline records.
					t.Logf("%s: deduplicated and saved %d tokens", shape.name, saved)
				}
				// The deduplicated body is a reference, so it must still name the
				// slice it stands for: the saving may never cost the model the
				// knowledge that it is looking at a reference.
				if !strings.Contains(strings.ToLower(secondBuilt), "reference") {
					t.Fatalf("%s replaced the body without saying so: %q", shape.name, secondBuilt)
				}
				return
			}
			// A shape that does not earn the benefit is recorded, not hidden: the
			// measurement is the input the plan asked for.
			if shape.knownGap == "" {
				t.Fatalf("%s does not deduplicate and no reason is recorded", shape.name)
			}
			_ = firstBuilt
			t.Logf("%s: no benefit today - %s", shape.name, shape.knownGap)
			if deduplicated != 0 {
				t.Fatalf("%s now deduplicates; the recorded gap is stale and the baseline must be updated", shape.name)
			}
		})
	}
}

func writeShape(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readTwice performs the same read twice through the production tool, so the
// measurement uses the metadata the tool really emits rather than a hand-written
// imitation of it.
func readTwice(t *testing.T, workspace, name string) []*protocol.ToolResult {
	t.Helper()
	// A generous byte limit: this measures deduplication, not truncation.
	reader, err := NewReadFile(workspace, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(map[string]any{"path": name})
	if err != nil {
		t.Fatal(err)
	}
	results := make([]*protocol.ToolResult, 0, 2)
	for attempt := 0; attempt < 2; attempt++ {
		result := reader.Execute(context.Background(), arguments)
		if result.IsError {
			t.Fatalf("read %d failed: %s", attempt+1, result.Content)
		}
		copied := result
		results = append(results, &copied)
	}
	return results
}
