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
	// wantReplaced is what the shape must earn today.
	wantReplaced bool
	// wantBenefit marks the shape whose token effect must be a win. A small body is
	// no longer replaced at all: the reference is very nearly a fixed size, so
	// replacing a body smaller than it would grow the request.
	wantBenefit bool
	// knownGap explains why a shape is not replaced.
	knownGap string
}

func dedupShapes() []dedupShape {
	var longLine strings.Builder
	for index := 0; index < 40_000; index++ {
		longLine.WriteString("x")
	}
	// Bodies below the reference size are deliberately kept: see
	// TestASmallBodyKeepsItsContent in internal/agent for the rule and
	// TestReadDeduplicationBenefitPerFileShape for what it is worth per shape.
	const smallReference = "the reference that would stand for this body is larger than the body"
	return []dedupShape{
		{name: "empty file", content: "", wantReplaced: false, knownGap: "there is no body to replace"},
		{name: "single line", content: "one line only\n", wantReplaced: false, knownGap: smallReference},
		{name: "no trailing newline", content: "one line without a terminator", wantReplaced: false, knownGap: smallReference},
		{name: "crlf and lf mixed", content: "first\r\nsecond\nthird\r\n", wantReplaced: false, knownGap: smallReference},
		{name: "unicode multibyte", content: "第一行：中文与 emoji 🙂\n第二行：更多多字节字符\n", wantReplaced: false, knownGap: smallReference},
		{
			// The long line is the shape the benefit is worth the most on: 40,001
			// bytes become a reference of a couple of hundred.
			//
			// A11's premise - that a body which cannot be kept whole by lines is
			// trimmed by bytes with no reported line ranges and therefore earns
			// nothing - is real, and it is reproduced in
			// internal/agent/long_line_dedup_test.go. It does not show up *here*,
			// because this case reads the file without trimming it: the shape is made
			// by the context budget, not by the read.
			name: "one very long line", content: longLine.String() + "\n", wantReplaced: true, wantBenefit: true,
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

			// The invariant the rule exists for: a repeated read may never cost more
			// than it did the first time. Before the rule, the four small shapes below
			// each grew by 106 to 159 tokens on the repeat.
			if saved < 0 {
				t.Fatalf("%s: the repeated read grew by %d tokens (%d -> %d)", shape.name, -saved, firstTokens, secondTokens)
			}
			if shape.wantBenefit && saved <= 0 {
				t.Fatalf("%s saved %d tokens although it is the shape the benefit exists for", shape.name, saved)
			}

			if shape.wantReplaced {
				if deduplicated == 0 {
					t.Fatalf("%s earned no deduplication benefit although it must: %#v", shape.name, result.SliceStats)
				}
				// The deduplicated body is a reference, so it must still name the
				// slice it stands for: the saving may never cost the model the
				// knowledge that it is looking at a reference.
				if !strings.Contains(strings.ToLower(secondBuilt), "reference") {
					t.Fatalf("%s replaced the body without saying so: %q", shape.name, secondBuilt)
				}
				return
			}
			// A shape that is not replaced says why, and its body must be untouched:
			// declining to replace is a decision about size, never a reason to lose
			// content.
			if shape.knownGap == "" {
				t.Fatalf("%s is not replaced and no reason is recorded", shape.name)
			}
			t.Logf("%s: kept as it is - %s", shape.name, shape.knownGap)
			if deduplicated != 0 {
				t.Fatalf("%s is now replaced; the recorded reason is stale and the baseline must be updated", shape.name)
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
