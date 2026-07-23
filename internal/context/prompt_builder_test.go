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

func TestPromptBuilderDeduplicatesAndPromotesCodeSlices(t *testing.T) {
	builder := NewPromptBuilder(ApproxEstimator{BytesPerToken: 1})
	addSlice := func(id, artifact, hash, content string, start, end int, cacheHit bool) {
		builder.AddTurn(protocol.Turn{ID: id, Role: protocol.RoleTool, Parts: []protocol.Part{{
			Kind: protocol.PartToolResult,
			ToolResult: &protocol.ToolResult{CallID: id, Content: content, Metadata: map[string]any{
				"relative_path": "main.go", "file_hash": hash, "artifact_id": artifact,
				"start_line": start, "end_line": end, "cache_hit": cacheHit,
			}},
		}}})
	}
	addSlice("first", "artifact-first", "hash-one", "lines 10 through 20", 10, 20, false)
	addSlice("contained", "artifact-contained", "hash-one", "lines 12 through 15", 12, 15, true)
	addSlice("promoted", "artifact-promoted", "hash-one", "lines 5 through 25", 5, 25, true)
	addSlice("stale", "artifact-stale", "hash-two", "new file generation", 1, 2, false)

	result := builder.Result()
	if result.SliceStats.CacheHits != 2 || result.SliceStats.Deduplicated != 2 || result.SliceStats.Stale != 1 {
		t.Fatalf("slice stats = %#v", result.SliceStats)
	}
	first := result.Turns[0].Parts[0].ToolResult.Content
	contained := result.Turns[1].Parts[0].ToolResult.Content
	promoted := result.Turns[2].Parts[0].ToolResult.Content
	if !strings.Contains(first, "artifact-promoted") || !strings.Contains(contained, "artifact-promoted") {
		t.Fatalf("references were not redirected: first=%q contained=%q", first, contained)
	}
	if promoted != "lines 5 through 25" || result.Turns[3].Parts[0].ToolResult.Content != "new file generation" {
		t.Fatalf("canonical content = %#v", result.Turns)
	}
	if result.Blocks[0].Category != CategoryCodeSlice || result.Blocks[1].Category != CategoryCodeSlice {
		t.Fatalf("blocks = %#v", result.Blocks)
	}
}

func TestPromptBuilderLeavesPartialOverlapAndTranscriptInputUntouched(t *testing.T) {
	first := &protocol.ToolResult{CallID: "first", Content: "full first", Metadata: map[string]any{
		"relative_path": "main.go", "file_hash": "hash", "artifact_id": "one", "start_line": 1, "end_line": 10,
	}}
	second := &protocol.ToolResult{CallID: "second", Content: "full second", Metadata: map[string]any{
		"relative_path": "main.go", "file_hash": "hash", "artifact_id": "two", "start_line": 8, "end_line": 15,
	}}
	builder := NewPromptBuilder(nil)
	builder.AddTurn(protocol.Turn{ID: "first", Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: first}}})
	builder.AddTurn(protocol.Turn{ID: "second", Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: second}}})
	result := builder.Result()
	if result.SliceStats.Deduplicated != 0 || result.Turns[0].Parts[0].ToolResult.Content != "full first" || result.Turns[1].Parts[0].ToolResult.Content != "full second" {
		t.Fatalf("result = %#v", result)
	}
	if first.Content != "full first" || second.Content != "full second" {
		t.Fatalf("input transcript was mutated: first=%q second=%q", first.Content, second.Content)
	}
}
