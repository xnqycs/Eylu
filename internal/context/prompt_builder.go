package context

import (
	"encoding/json"
	"fmt"
	"strings"

	"Eylu/internal/protocol"
)

type PromptResult struct {
	Turns  []protocol.Turn
	Tools  []protocol.ToolDefinition
	Blocks []Block
}

func (r PromptResult) InputTokens() int {
	total := 0
	for _, block := range r.Blocks {
		if block.Category != CategoryOutputReserve {
			total += block.Tokens
		}
	}
	return total
}

type PromptBuilder struct {
	estimator TokenEstimator
	result    PromptResult
}

func NewPromptBuilder(estimator TokenEstimator) *PromptBuilder {
	if estimator == nil {
		estimator = ApproxEstimator{BytesPerToken: 4}
	}
	return &PromptBuilder{estimator: estimator}
}

func (b *PromptBuilder) AddTextTurn(id string, role protocol.Role, text string, category Category, source string, protected bool, metadata map[string]any) {
	if text == "" {
		return
	}
	b.result.Turns = append(b.result.Turns, protocol.Turn{ID: id, Role: role, Parts: []protocol.Part{{Kind: protocol.PartText, Text: text}}})
	b.addTextBlock(id+":0", category, source, text, protected, metadata)
}

func (b *PromptBuilder) AddTurn(turn protocol.Turn) {
	turn.Parts = append([]protocol.Part(nil), turn.Parts...)
	b.result.Turns = append(b.result.Turns, turn)
	for index, part := range turn.Parts {
		id := fmt.Sprintf("%s:%d", turn.ID, index)
		switch {
		case part.Kind == protocol.PartText || part.Kind == protocol.PartReasoning:
			category := CategoryAgentMessage
			if turn.Role == protocol.RoleUser {
				category = CategoryUserMessage
			}
			b.addTextBlock(id, category, turn.ID, part.Text, false, map[string]any{"part_kind": part.Kind})
		case part.Kind == protocol.PartToolCall && part.ToolCall != nil:
			content := part.ToolCall.Name + "\n" + string(part.ToolCall.Arguments)
			b.addTextBlock(id, CategoryAgentMessage, part.ToolCall.Name, content, false, map[string]any{"call_id": part.ToolCall.ID, "part_kind": part.Kind})
		case part.Kind == protocol.PartToolResult && part.ToolResult != nil:
			category, source := toolResultCategory(part.ToolResult)
			metadata := map[string]any{"call_id": part.ToolResult.CallID, "is_error": part.ToolResult.IsError, "truncated": part.ToolResult.Truncated}
			b.addTextBlock(id, category, source, part.ToolResult.Content, false, metadata)
		}
	}
}

func (b *PromptBuilder) AddTools(definitions []protocol.ToolDefinition, category Category, sourcePrefix string) {
	for _, definition := range definitions {
		b.result.Tools = append(b.result.Tools, definition)
		encoded, _ := json.Marshal(definition)
		source := definition.Name
		if sourcePrefix != "" {
			source = sourcePrefix + ":" + definition.Name
		}
		b.addTextBlock("tool-schema:"+source, category, source, string(encoded), true, nil)
	}
}

func (b *PromptBuilder) AddDriverState(source string, state json.RawMessage) {
	if len(state) > 0 {
		b.addTextBlock("driver-state", CategoryDriverState, source, string(state), false, nil)
	}
}

func (b *PromptBuilder) SetOutputReserve(tokens int) {
	if tokens > 0 {
		b.result.Blocks = append(b.result.Blocks, Block{ID: "output-reserve", Category: CategoryOutputReserve, Source: "runtime", Tokens: tokens, Exact: false})
	}
}

func (b *PromptBuilder) Result() PromptResult {
	result := b.result
	result.Turns = append([]protocol.Turn(nil), result.Turns...)
	result.Tools = append([]protocol.ToolDefinition(nil), result.Tools...)
	result.Blocks = append([]Block(nil), result.Blocks...)
	return result
}

func (b *PromptBuilder) addTextBlock(id string, category Category, source, text string, protected bool, metadata map[string]any) {
	b.result.Blocks = append(b.result.Blocks, Block{
		ID: id, Category: category, Source: source, Bytes: len([]byte(text)), Tokens: b.estimator.Estimate(text), Exact: false, Protected: protected, Metadata: metadata,
	})
}

func toolResultCategory(result *protocol.ToolResult) (Category, string) {
	if result.Metadata != nil {
		if server, ok := result.Metadata["mcp_server"].(string); ok && server != "" {
			return CategoryMCPToolResult, server
		}
		if name, ok := result.Metadata["skill_name"].(string); ok && name != "" {
			if resource, ok := result.Metadata["resource"].(string); ok && resource != "" {
				return CategorySkillResource, name + ":" + resource
			}
		}
	}
	return CategoryBuiltinToolResult, result.CallID
}

func PaginateSkillCatalog(catalog string, maxPayloadBytes int) []string {
	trimmed := strings.TrimSpace(catalog)
	if trimmed == "" {
		return nil
	}
	if maxPayloadBytes <= 0 {
		maxPayloadBytes = 8 << 10
	}
	lines := strings.Split(trimmed, "\n")
	items := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "<skill>") {
			items = append(items, line)
		}
	}
	if len(items) == 0 {
		items = []string{trimmed}
	}
	payloads := make([][]string, 1)
	for _, item := range items {
		current := payloads[len(payloads)-1]
		used := len(strings.Join(current, "\n"))
		if len(current) > 0 && used+1+len(item) > maxPayloadBytes {
			payloads = append(payloads, nil)
		}
		payloads[len(payloads)-1] = append(payloads[len(payloads)-1], item)
	}
	pages := make([]string, 0, len(payloads))
	for index, payload := range payloads {
		pages = append(pages, fmt.Sprintf("<available_skills page=\"%d\" pages=\"%d\">\n  %s\n</available_skills>", index+1, len(payloads), strings.Join(payload, "\n  ")))
	}
	return pages
}
