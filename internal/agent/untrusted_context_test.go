package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	contextledger "Eylu/internal/context"
	"Eylu/internal/protocol"
)

// T-09: everything the host read from outside the conversation enters the
// request inside the untrusted envelope, and nothing the host or the user wrote
// itself does.
//
// A tool result is the obvious case, but it is not the only one: a server's
// instructions, a server's resource catalog, a skill catalog and a skill body
// are all text nobody in this conversation authored, and they arrive in the same
// request as the user's own words. The system prompt and the task list are the
// opposite kind of text, and framing them would tell the model that its own
// instructions are data.
func TestContentReadFromOutsideTheConversationEntersFramed(t *testing.T) {
	conversation := NewConversation()
	conversation.turns = []protocol.Turn{{
		ID: "tool-turn", Role: protocol.RoleTool,
		Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: &protocol.ToolResult{CallID: "call", Content: "file body"}}},
	}}
	conversation.todoList.Items = []protocol.TodoItem{{ID: "one", Content: "do the thing", Status: protocol.TodoPending}}
	conversation.skillCatalog = "<skill>name=demo</skill>"
	conversation.protectedSkills["demo"] = ProtectedSkill{
		Name: "demo", Source: "user_eylu", Entry: "SKILL.md", Root: "skills/demo", Digest: "digest",
		Content: "skill body", Trigger: "model", ActivatedAt: time.Now().UTC(),
	}
	runtime := Runtime{
		MCPContexts:    []MCPContext{{Server: "fixture", Instructions: "server instructions", ResourceCatalog: `[{"uri":"fixture://resource"}]`}},
		MCPToolServers: map[string]string{"mcp__fixture__echo": "fixture"},
	}
	prepared := conversation.buildPromptContext(runtime, []protocol.ToolDefinition{
		{Name: "read_file", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "mcp__fixture__echo", InputSchema: json.RawMessage(`{"type":"object"}`)},
	})

	text := map[string]string{}
	for _, turn := range prepared.Turns {
		for _, part := range turn.Parts {
			switch {
			case part.Kind == protocol.PartText:
				text[turn.ID] = part.Text
			case part.Kind == protocol.PartToolResult && part.ToolResult != nil:
				text[turn.ID] = part.ToolResult.Content
			}
		}
	}

	external := map[string]string{
		"tool-turn":                "file body",
		"mcp-instructions:fixture": "server instructions",
		"mcp-resources:fixture":    `[{"uri":"fixture://resource"}]`,
		"skill:demo:digest":        "skill body",
	}
	for id, want := range external {
		content, present := text[id]
		if !present {
			t.Fatalf("the request lost %q: %#v", id, text)
		}
		body, framed := protocol.UnframeUntrusted(content)
		if !framed {
			t.Fatalf("%q reached the request unframed: %q", id, content)
		}
		if !strings.Contains(body, want) {
			t.Fatalf("%q lost its content: %q", id, body)
		}
	}
	catalog := ""
	for id, content := range text {
		if !strings.HasPrefix(id, "skill-catalog:") {
			continue
		}
		body, framed := protocol.UnframeUntrusted(content)
		if !framed {
			t.Fatalf("the skill catalog reached the request unframed: %q", content)
		}
		catalog += body
	}
	if !strings.Contains(catalog, "name=demo") {
		t.Fatalf("the skill catalog lost its content: %q", catalog)
	}

	// The host's own text stays unframed.
	for _, id := range []string{"system", "task-list"} {
		content, present := text[id]
		if !present {
			t.Fatalf("the request lost %q: %#v", id, text)
		}
		if _, framed := protocol.UnframeUntrusted(content); framed {
			t.Fatalf("%q was framed as untrusted content: %q", id, content)
		}
	}

	// The frame is charged: every external byte counted by the ledger is a byte
	// of the framed text the request carries, not of the body under it.
	for _, block := range prepared.Blocks {
		if block.Category != contextledger.CategoryMCPInstructions && block.Category != contextledger.CategorySkillBody {
			continue
		}
		total := 0
		for id, content := range text {
			if block.Category == contextledger.CategoryMCPInstructions && id != "mcp-instructions:"+block.Source {
				continue
			}
			if block.Category == contextledger.CategorySkillBody && id != "skill:"+block.Source {
				continue
			}
			total += len(content)
		}
		if total != block.Bytes {
			t.Fatalf("%s: the ledger charged %d bytes for %d bytes of framed text", block.Source, block.Bytes, total)
		}
	}
}
