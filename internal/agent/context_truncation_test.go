package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"Eylu/internal/protocol"
)

func sliceResult(callID, artifact, body string, start, end int) *protocol.ToolResult {
	return &protocol.ToolResult{CallID: callID, Content: body, Metadata: map[string]any{
		"relative_path": "main.go", "path": "main.go", "file_hash": "hash-one", "artifact_id": artifact,
		"start_line": start, "end_line": end, "lines_complete": true, "bytes": len(body),
	}}
}

// A tool result trimmed for the context window must declare itself incomplete,
// and the full transcript must stay untouched.
func TestContextualizeTurnMarksTrimmedCodeSliceIncomplete(t *testing.T) {
	body := strings.Repeat("// filler\n", 200) + "TAIL-MARKER"
	result := sliceResult("call", "big", body, 1, 1000)
	turn := protocol.Turn{ID: "tool", Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: result}}}

	contextTurn, keep := contextualizeTurn(turn, 240)
	if !keep {
		t.Fatal("trimmed turn was dropped")
	}
	trimmed := contextTurn.Parts[0].ToolResult
	if !trimmed.Truncated {
		t.Fatal("trimmed copy did not report truncation")
	}
	if trimmed.Metadata["context_truncated"] != true {
		t.Fatalf("metadata = %#v", trimmed.Metadata)
	}
	if trimmed.Metadata["lines_complete"] != false {
		t.Fatalf("metadata = %#v", trimmed.Metadata)
	}
	if len(trimmed.Content) > 240 || !strings.Contains(trimmed.Content, "TAIL-MARKER") {
		t.Fatalf("content = %q", trimmed.Content)
	}
	if !utf8.ValidString(trimmed.Content) {
		t.Fatal("trimmed content is not valid UTF-8")
	}
	if turn.Parts[0].ToolResult.Content != body || turn.Parts[0].ToolResult.Truncated {
		t.Fatal("the full transcript result was mutated")
	}
}

// An untrimmed result keeps its completeness markers.
func TestContextualizeTurnLeavesCompleteSliceMarkedComplete(t *testing.T) {
	body := "complete body"
	turn := protocol.Turn{ID: "tool", Role: protocol.RoleTool, Parts: []protocol.Part{{
		Kind: protocol.PartToolResult, ToolResult: sliceResult("call", "small", body, 10, 20),
	}}}
	contextTurn, _ := contextualizeTurn(turn, 4096)
	result := contextTurn.Parts[0].ToolResult
	if result.Truncated || result.Metadata["context_truncated"] != nil || result.Content != body {
		t.Fatalf("result = %#v", result)
	}
}

// After the context layer trims a large read, a later read of the omitted middle
// must still carry its body, in either order.
func TestPromptContextKeepsMiddleBodyAfterContextTrimming(t *testing.T) {
	filler := strings.Repeat("// filler line\n", 200) + "TAIL-MARKER"
	for _, test := range []struct {
		name   string
		first  *protocol.ToolResult
		second *protocol.ToolResult
	}{
		{
			name:   "big read first",
			first:  sliceResult("big", "big", filler, 1, 1000),
			second: sliceResult("middle", "middle", "MIDDLE-BODY-MARKER", 400, 450),
		},
		{
			name:   "middle read first",
			first:  sliceResult("middle", "middle", "MIDDLE-BODY-MARKER", 400, 450),
			second: sliceResult("big", "big", filler, 1, 1000),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			conversation := NewConversation()
			conversation.turns = []protocol.Turn{
				{ID: "user", Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "read"}}},
				{ID: "agent-1", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartToolCall, ToolCall: &protocol.ToolCall{ID: test.first.CallID, Name: "read_file", Arguments: json.RawMessage(`{}`)}}}},
				{ID: "tool-1", Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: test.first}}},
				{ID: "agent-2", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartToolCall, ToolCall: &protocol.ToolCall{ID: test.second.CallID, Name: "read_file", Arguments: json.RawMessage(`{}`)}}}},
				{ID: "tool-2", Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: test.second}}},
			}
			runtime := testRuntime(&loopDriver{}, 1)
			runtime.MaxToolContextBytes = 240
			prepared := conversation.buildPromptContext(runtime, nil)

			middle := findToolResult(prepared.Turns, "middle")
			if middle == nil {
				t.Fatalf("middle result missing from the request")
			}
			if !strings.Contains(middle.Content, "MIDDLE-BODY-MARKER") {
				t.Fatalf("middle body was replaced: %q", middle.Content)
			}
			if strings.Contains(middle.Content, "code slice reference") {
				t.Fatalf("middle result is a reference: %q", middle.Content)
			}
			// The stored transcript keeps the original bodies.
			stored := conversation.Transcript()
			if got := findToolResult(stored, "big"); got == nil || got.Content != filler {
				t.Fatalf("stored big read was rewritten")
			}
			if got := findToolResult(stored, "middle"); got == nil || got.Content != "MIDDLE-BODY-MARKER" {
				t.Fatalf("stored middle read was rewritten")
			}
		})
	}
}

func findToolResult(turns []protocol.Turn, callID string) *protocol.ToolResult {
	for _, turn := range turns {
		for _, part := range turn.Parts {
			if part.ToolResult != nil && part.ToolResult.CallID == callID {
				return part.ToolResult
			}
		}
	}
	return nil
}
