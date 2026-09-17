package context

import (
	"encoding/json"
	"strings"
	"testing"

	"Eylu/internal/protocol"
)

// The ledger charges for what the request carries.
//
// A tool result can hold text, multimodal blocks and structured JSON while a
// request can carry one text field, so the block the ledger counts has to be the
// projection the driver will send. Counting the text field alone understates a
// rich result by everything the driver adds on its own.
func TestTheLedgerCountsTheProjectionOfARichToolResult(t *testing.T) {
	result := protocol.ToolResult{
		CallID: "call-1", Content: "legacy text",
		ContentBlocks:     []protocol.ContentBlock{{Type: protocol.ContentText, Text: strings.Repeat("b", 3<<13)}},
		StructuredContent: json.RawMessage(`{"count":2,"detail":"` + strings.Repeat("d", 3<<13) + `"}`),
	}
	projection := protocol.ProjectToolResult(result)
	if !projection.Omitted {
		t.Fatal("the fixture is not rich enough to be projected")
	}

	builder := NewPromptBuilder(ApproxEstimator{BytesPerToken: 4})
	builder.AddTurn(protocol.Turn{ID: "tools", Role: protocol.RoleTool, Parts: []protocol.Part{
		{Kind: protocol.PartToolResult, ToolResult: &result},
	}})
	built := builder.Result()
	if len(built.Turns) != 1 || built.Turns[0].Parts[0].ToolResult == nil {
		t.Fatalf("built = %#v", built.Turns)
	}
	sent := built.Turns[0].Parts[0].ToolResult
	if sent.ContentBlocks != nil || sent.StructuredContent != nil {
		t.Fatalf("the request still carries the stored rich fields: %#v", sent)
	}
	if sent.Content != projection.Text {
		t.Fatalf("the request carries something other than the projection:\n got %q\nwant %q", sent.Content, projection.Text)
	}
	if !sent.Truncated {
		t.Fatal("a projected result was presented as complete")
	}

	// The ledger's tool-result blocks add up to the projection, and nothing else.
	counted := 0
	for _, block := range built.Blocks {
		if block.Category != CategoryBuiltinToolResult && block.Category != CategoryMCPToolResult {
			continue
		}
		counted += block.Bytes
	}
	if counted != len(projection.Text) {
		t.Fatalf("the ledger counted %d bytes of a %d byte projection", counted, len(projection.Text))
	}

	// And the driver sends exactly what was counted.
	if driverText := protocol.ToolResultText(*sent); driverText != projection.Text {
		t.Fatalf("the driver would send %d bytes for a counted %d", len(driverText), len(projection.Text))
	}
}
