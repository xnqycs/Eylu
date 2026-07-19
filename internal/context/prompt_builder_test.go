package context

import (
	"encoding/json"
	"strings"
	"testing"

	"Eylu/internal/protocol"
)

func TestPromptBuilderUsesActualInputForBlocks(t *testing.T) {
	builder := NewPromptBuilder(ApproxEstimator{BytesPerToken: 1})
	builder.AddTextTurn("system", protocol.RoleSystem, "system", CategorySystemPrompt, "eylu", true, nil)
	builder.AddTurn(protocol.Turn{ID: "user", Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "question"}}})
	builder.AddTurn(protocol.Turn{
		ID: "tool", Role: protocol.RoleTool,
		Parts: []protocol.Part{{
			Kind: protocol.PartToolResult,
			ToolResult: &protocol.ToolResult{CallID: "call", Content: "resource", Metadata: map[string]any{
				"skill_name": "demo", "resource": "references/guide.md",
			}},
		}},
	})
	builder.AddTools([]protocol.ToolDefinition{{Name: "read", Description: "Read", InputSchema: json.RawMessage(`{"type":"object"}`)}}, CategoryBuiltinToolSchema, "builtin")
	builder.AddDriverState("provider", json.RawMessage(`{"id":"response"}`))
	builder.SetOutputReserve(10)
	result := builder.Result()
	if len(result.Turns) != 3 || len(result.Tools) != 1 || result.InputTokens() == 0 {
		t.Fatalf("result = %#v", result)
	}
	foundResource := false
	for _, block := range result.Blocks {
		foundResource = foundResource || block.Category == CategorySkillResource && block.Source == "demo:references/guide.md"
	}
	if !foundResource {
		t.Fatalf("blocks = %#v", result.Blocks)
	}
}

func TestPaginateSkillCatalogIsStableAndComplete(t *testing.T) {
	catalog := "<available_skills>\n  <skill><name>a</name><description>alpha</description><source>user</source></skill>\n  <skill><name>b</name><description>beta</description><source>user</source></skill>\n</available_skills>"
	first := PaginateSkillCatalog(catalog, 90)
	second := PaginateSkillCatalog(catalog, 90)
	if len(first) != 2 || strings.Join(first, "") != strings.Join(second, "") || !strings.Contains(first[0], `page="1" pages="2"`) || !strings.Contains(first[1], "<name>b</name>") {
		t.Fatalf("pages = %#v", first)
	}
}
