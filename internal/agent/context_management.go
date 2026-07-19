package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	contextledger "Eylu/internal/context"
	"Eylu/internal/protocol"
)

const skillCatalogInstructions = "The following skills provide specialized instructions. When a task matches a description, call activate_skill with its name before proceeding. Read skill resources with read_skill_resource after activation."

type contextOptions struct {
	estimator        contextledger.TokenEstimator
	outputReserve    int
	recentRounds     int
	projectMapBytes  int
	toolContextBytes int
	catalogPageBytes int
	summaryBytes     int
}

func optionsForRuntime(runtime Runtime) contextOptions {
	options := contextOptions{
		estimator: runtime.TokenEstimator, outputReserve: runtime.OutputReserveTokens, recentRounds: runtime.ContextRecentRounds,
		projectMapBytes: runtime.MaxProjectMapBytes, toolContextBytes: runtime.MaxToolContextBytes,
		catalogPageBytes: runtime.SkillCatalogPageBytes, summaryBytes: runtime.MaxSummaryBytes,
	}
	if options.estimator == nil {
		options.estimator = contextledger.ApproxEstimator{BytesPerToken: 4}
	}
	if options.outputReserve <= 0 {
		options.outputReserve = 8192
	}
	if window := runtime.Provider.Config.ContextWindow; window > 0 && options.outputReserve >= window {
		options.outputReserve = max(1, window/4)
	}
	if options.recentRounds <= 0 {
		options.recentRounds = 3
	}
	if options.projectMapBytes <= 0 {
		options.projectMapBytes = contextledger.DefaultProjectMapBytes
	}
	if options.toolContextBytes <= 0 {
		options.toolContextBytes = 8 << 10
	}
	if options.catalogPageBytes <= 0 {
		options.catalogPageBytes = 8 << 10
	}
	if options.summaryBytes <= 0 {
		options.summaryBytes = 16 << 10
	}
	return options
}

func (c *Conversation) prepareRequestContext(runtime Runtime, definitions []protocol.ToolDefinition) (contextledger.PromptResult, error) {
	options := optionsForRuntime(runtime)
	c.ledger.SetEstimator(options.estimator)
	c.refreshProjectMap(runtime)
	prepared := c.buildPromptContext(runtime, definitions)
	window := runtime.Provider.Config.ContextWindow
	before := prepared.InputTokens()
	omittedBefore := len(c.omittedTurnIDs)
	for window > 0 && prepared.InputTokens()+options.outputReserve > window {
		candidates := c.compressionCandidates(options.recentRounds)
		if len(candidates) == 0 {
			prepared = c.finalizeCompression(runtime, definitions, options, prepared, before, omittedBefore)
			return contextledger.PromptResult{}, &protocol.Error{Code: protocol.ErrProtocol, Message: fmt.Sprintf("context budget exceeded: %d input + %d reserved > %d", prepared.InputTokens(), options.outputReserve, window)}
		}
		for _, id := range candidates[0] {
			c.omittedTurnIDs[id] = struct{}{}
		}
		c.summary = c.buildSummary(options.summaryBytes)
		prepared = c.buildPromptContext(runtime, definitions)
	}
	prepared = c.finalizeCompression(runtime, definitions, options, prepared, before, omittedBefore)
	if runtime.ContextEvent != nil {
		percent := 0.0
		if window > 0 {
			percent = float64(prepared.InputTokens()+options.outputReserve) / float64(window) * 100
		}
		runtime.ContextEvent(contextledger.Event{Kind: contextledger.EventBudget, InputTokens: prepared.InputTokens(), OutputReserve: options.outputReserve, ContextWindow: window, Percent: percent})
	}
	return prepared, nil
}

func (c *Conversation) finalizeCompression(runtime Runtime, definitions []protocol.ToolDefinition, options contextOptions, prepared contextledger.PromptResult, before, omittedBefore int) contextledger.PromptResult {
	if len(c.omittedTurnIDs) > omittedBefore {
		c.driverState = nil
		prepared = c.buildPromptContext(runtime, definitions)
		event := contextledger.CompressionEvent{
			BeforeTokens: before, AfterTokens: prepared.InputTokens(), OmittedTurns: len(c.omittedTurnIDs), SummaryBytes: len([]byte(c.summary)), OccurredAt: time.Now().UTC(),
		}
		c.ledger.RecordCompression(event)
		if runtime.ContextEvent != nil {
			runtime.ContextEvent(contextledger.Event{Kind: contextledger.EventCompression, InputTokens: prepared.InputTokens(), OutputReserve: options.outputReserve, ContextWindow: runtime.Provider.Config.ContextWindow, Compression: &event})
		}
	}
	c.ledger.ReplaceBlocks(prepared.Blocks)
	return prepared
}

func (c *Conversation) buildPromptContext(runtime Runtime, definitions []protocol.ToolDefinition) contextledger.PromptResult {
	options := optionsForRuntime(runtime)
	builder := contextledger.NewPromptBuilder(options.estimator)
	systemPrompt := c.systemPrompt
	if environmentPrompt := c.environment.Prompt(runtime.Provider.Config.Model); environmentPrompt != "" {
		systemPrompt += "\n\n" + environmentPrompt
	}
	builder.AddTextTurn("system", protocol.RoleSystem, systemPrompt, contextledger.CategorySystemPrompt, "eylu", true, nil)
	for _, server := range runtime.MCPContexts {
		if server.Instructions != "" {
			content := fmt.Sprintf("<mcp_instructions server=%q>\n%s\n</mcp_instructions>", server.Server, server.Instructions)
			builder.AddTextTurn("mcp-instructions:"+server.Server, protocol.RoleSystem, content, contextledger.CategoryMCPInstructions, server.Server, true, map[string]any{"server": server.Server})
		}
		if server.ResourceCatalog != "" {
			content := fmt.Sprintf("<mcp_resources server=%q>\n%s\n</mcp_resources>", server.Server, server.ResourceCatalog)
			builder.AddTextTurn("mcp-resources:"+server.Server, protocol.RoleSystem, content, contextledger.CategoryMCPResource, server.Server, true, map[string]any{"server": server.Server})
		}
	}
	pages := contextledger.PaginateSkillCatalog(c.skillCatalog, options.catalogPageBytes)
	for index, page := range pages {
		content := page
		if index == 0 {
			content = skillCatalogInstructions + "\n" + page
		}
		source := fmt.Sprintf("page:%d/%d", index+1, len(pages))
		builder.AddTextTurn("skill-catalog:"+source, protocol.RoleSystem, content, contextledger.CategorySkillCatalog, source, true, map[string]any{"page": index + 1, "pages": len(pages)})
	}
	for _, name := range protectedNamesFromMap(c.protectedSkills) {
		protected := c.protectedSkills[name]
		builder.AddTextTurn("skill:"+name+":"+protected.Digest, protocol.RoleSystem, protected.Content, contextledger.CategorySkillBody, name+":"+protected.Digest, true, map[string]any{"name": name, "source": protected.Source, "digest": protected.Digest})
	}
	if c.projectMap != "" {
		builder.AddTextTurn("project-map", protocol.RoleSystem, c.projectMap, contextledger.CategoryProjectContext, runtime.Workspace, true, nil)
	}
	if c.summary != "" {
		builder.AddTextTurn("conversation-summary", protocol.RoleSystem, c.summary, contextledger.CategorySummary, "compression", true, nil)
	}
	for _, turn := range c.turns {
		if _, omitted := c.omittedTurnIDs[turn.ID]; omitted {
			continue
		}
		contextTurn, keep := contextualizeTurn(turn, options.toolContextBytes)
		if keep {
			builder.AddTurn(contextTurn)
		}
	}
	builtinDefinitions := make([]protocol.ToolDefinition, 0, len(definitions))
	mcpDefinitions := make(map[string][]protocol.ToolDefinition)
	for _, definition := range definitions {
		if server := runtime.MCPToolServers[definition.Name]; server != "" {
			mcpDefinitions[server] = append(mcpDefinitions[server], definition)
		} else {
			builtinDefinitions = append(builtinDefinitions, definition)
		}
	}
	builder.AddTools(builtinDefinitions, contextledger.CategoryBuiltinToolSchema, "builtin")
	mcpServers := make([]string, 0, len(mcpDefinitions))
	for server := range mcpDefinitions {
		mcpServers = append(mcpServers, server)
	}
	sort.Strings(mcpServers)
	for _, server := range mcpServers {
		builder.AddTools(mcpDefinitions[server], contextledger.CategoryMCPToolSchema, server)
	}
	builder.AddDriverState(runtime.Provider.Name, c.driverState)
	builder.SetOutputReserve(options.outputReserve)
	return builder.Result()
}

func (c *Conversation) refreshProjectMap(runtime Runtime) {
	workspace := strings.TrimSpace(runtime.Workspace)
	if workspace == "" {
		c.projectMap = ""
		c.projectMapWorkspace = ""
		return
	}
	maxBytes := optionsForRuntime(runtime).projectMapBytes
	if !c.projectMapDirty && c.projectMapWorkspace == workspace && c.projectMapMaxBytes == maxBytes {
		return
	}
	projectMap, err := contextledger.BuildProjectMap(workspace, maxBytes)
	if err != nil {
		c.projectMap = ""
	} else {
		c.projectMap = projectMap.Content
	}
	c.projectMapWorkspace = workspace
	c.projectMapMaxBytes = maxBytes
	c.projectMapDirty = false
}

func contextualizeTurn(turn protocol.Turn, maxToolBytes int) (protocol.Turn, bool) {
	result := turn
	result.Parts = make([]protocol.Part, 0, len(turn.Parts))
	for _, part := range turn.Parts {
		if part.Kind == protocol.PartReasoning {
			continue
		}
		copy := part
		if part.ToolCall != nil {
			call := *part.ToolCall
			call.Arguments = append(json.RawMessage(nil), part.ToolCall.Arguments...)
			copy.ToolCall = &call
		}
		if part.ToolResult != nil {
			toolResult := *part.ToolResult
			toolResult.Metadata = cloneMetadata(part.ToolResult.Metadata)
			toolResult.Content = contextSnippet(part.ToolResult.Content, maxToolBytes)
			copy.ToolResult = &toolResult
		}
		result.Parts = append(result.Parts, copy)
	}
	return result, len(result.Parts) > 0
}

func contextSnippet(content string, limit int) string {
	if limit <= 0 || len(content) <= limit {
		return content
	}
	marker := fmt.Sprintf("\n[tool result summarized: original_bytes=%d]\n", len(content))
	available := limit - len(marker)
	if available <= 0 {
		return marker[:min(len(marker), limit)]
	}
	head := available * 2 / 3
	tail := available - head
	for head > 0 && !utf8.RuneStart(content[head]) {
		head--
	}
	start := len(content) - tail
	for start < len(content) && !utf8.RuneStart(content[start]) {
		start++
	}
	return content[:head] + marker + content[start:]
}

func cloneMetadata(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (c *Conversation) compressionCandidates(recentRounds int) [][]string {
	rounds := conversationRounds(c.turns)
	candidates := make([][]string, 0)
	oldCount := max(0, len(rounds)-recentRounds)
	for index := 0; index < oldCount; index++ {
		if group := c.activeTurnIDs(rounds[index]); len(group) > 0 {
			candidates = append(candidates, group)
		}
	}
	for index := oldCount; index < len(rounds); index++ {
		pairs := c.activeToolPairs(rounds[index])
		for pairIndex := 0; pairIndex+2 < len(pairs); pairIndex++ {
			candidates = append(candidates, pairs[pairIndex])
		}
	}
	for index := oldCount; index+1 < len(rounds); index++ {
		if group := c.activeTurnIDs(rounds[index]); len(group) > 0 {
			candidates = append(candidates, group)
		}
	}
	for index := oldCount; index < len(rounds); index++ {
		pairs := c.activeToolPairs(rounds[index])
		for pairIndex := 0; pairIndex+1 < len(pairs); pairIndex++ {
			candidates = append(candidates, pairs[pairIndex])
		}
	}
	if len(rounds) == 0 && len(c.turns) > 1 {
		if group := c.activeTurnIDs(c.turns[:len(c.turns)-1]); len(group) > 0 {
			candidates = append(candidates, group)
		}
	}
	return candidates
}

func conversationRounds(turns []protocol.Turn) [][]protocol.Turn {
	rounds := make([][]protocol.Turn, 0)
	for _, turn := range turns {
		if turn.Role == protocol.RoleUser || len(rounds) == 0 {
			rounds = append(rounds, nil)
		}
		rounds[len(rounds)-1] = append(rounds[len(rounds)-1], turn)
	}
	return rounds
}

func (c *Conversation) activeTurnIDs(turns []protocol.Turn) []string {
	ids := make([]string, 0, len(turns))
	for _, turn := range turns {
		if _, omitted := c.omittedTurnIDs[turn.ID]; !omitted {
			ids = append(ids, turn.ID)
		}
	}
	return ids
}

func (c *Conversation) activeToolPairs(round []protocol.Turn) [][]string {
	pairs := make([][]string, 0)
	for index := 0; index+1 < len(round); index++ {
		agentTurn, toolTurn := round[index], round[index+1]
		if _, omitted := c.omittedTurnIDs[agentTurn.ID]; omitted {
			continue
		}
		if _, omitted := c.omittedTurnIDs[toolTurn.ID]; omitted {
			continue
		}
		if agentTurn.Role == protocol.RoleAgent && toolTurn.Role == protocol.RoleTool && len(toolCalls(agentTurn)) > 0 {
			pairs = append(pairs, []string{agentTurn.ID, toolTurn.ID})
			index++
		}
	}
	return pairs
}

func (c *Conversation) buildSummary(limit int) string {
	type callInfo struct {
		name string
		args json.RawMessage
	}
	calls := make(map[string]callInfo)
	goals := make([]string, 0)
	completed := make([]string, 0)
	failures := make([]string, 0)
	validation := make([]string, 0)
	notes := make([]string, 0)
	for _, turn := range c.turns {
		_, omitted := c.omittedTurnIDs[turn.ID]
		if !omitted && turn.Role == protocol.RoleUser {
			for _, part := range turn.Parts {
				if part.Kind == protocol.PartText {
					goals = appendUnique(goals, compactSummaryText(part.Text, 500), 20)
				}
			}
		}
		if !omitted {
			continue
		}
		for _, part := range turn.Parts {
			switch {
			case part.Kind == protocol.PartText && turn.Role == protocol.RoleUser:
				goals = appendUnique(goals, compactSummaryText(part.Text, 500), 20)
			case part.Kind == protocol.PartText && turn.Role == protocol.RoleAgent:
				notes = appendUnique(notes, compactSummaryText(part.Text, 500), 20)
			case part.Kind == protocol.PartToolCall && part.ToolCall != nil:
				calls[part.ToolCall.ID] = callInfo{name: part.ToolCall.Name, args: part.ToolCall.Arguments}
				if isModificationTool(part.ToolCall.Name) {
					completed = appendUnique(completed, modificationSummary(part.ToolCall.Name, part.ToolCall.Arguments), 100)
				}
			case part.Kind == protocol.PartToolResult && part.ToolResult != nil:
				call := calls[part.ToolResult.CallID]
				line := compactSummaryText(part.ToolResult.Content, 500)
				if part.ToolResult.IsError {
					failures = appendUnique(failures, call.name+": "+line, 50)
				} else if call.name == "bash" {
					validation = appendUnique(validation, line, 50)
				}
			}
		}
	}
	var output strings.Builder
	output.WriteString("<conversation_summary>\nUser goals:\n")
	writeSummaryItems(&output, goals, "No earlier user goal was compressed.")
	output.WriteString("Completed modifications and decisions:\n")
	writeSummaryItems(&output, append(completed, notes...), "No completed modification was recorded.")
	output.WriteString("Unfinished tasks:\n- Continue the latest retained user goal and pending tool workflow.\n")
	output.WriteString("Failed attempts:\n")
	writeSummaryItems(&output, failures, "No failed attempt was recorded.")
	output.WriteString("Validation results:\n")
	writeSummaryItems(&output, validation, "No validation result was recorded.")
	output.WriteString("Activated skills:\n")
	for _, name := range protectedNamesFromMap(c.protectedSkills) {
		item := c.protectedSkills[name]
		fmt.Fprintf(&output, "- %s source=%s digest=%s\n", item.Name, item.Source, item.Digest)
	}
	if len(c.protectedSkills) == 0 {
		output.WriteString("- none\n")
	}
	output.WriteString("</conversation_summary>")
	return truncateSummary(output.String(), limit)
}

func appendUnique(items []string, value string, limit int) []string {
	if value == "" || len(items) >= limit {
		return items
	}
	for _, item := range items {
		if item == value {
			return items
		}
	}
	return append(items, value)
}

func writeSummaryItems(output *strings.Builder, items []string, empty string) {
	if len(items) == 0 {
		fmt.Fprintf(output, "- %s\n", empty)
		return
	}
	for _, item := range items {
		fmt.Fprintf(output, "- %s\n", item)
	}
}

func compactSummaryText(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "..."
}

func isModificationTool(name string) bool {
	return name == "write_file" || name == "edit_file"
}

func modificationSummary(name string, arguments json.RawMessage) string {
	var values map[string]any
	_ = json.Unmarshal(arguments, &values)
	path, _ := values["path"].(string)
	if path == "" {
		path, _ = values["file_path"].(string)
	}
	if path == "" {
		return name + " completed"
	}
	return name + " " + path
}

func truncateSummary(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	marker := "\n[summary truncated]\n</conversation_summary>"
	if limit <= len(marker) {
		return marker[:limit]
	}
	end := max(0, limit-len(marker))
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + marker
}

func (c *Conversation) ContextSummary() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.summary
}

func (c *Conversation) RetainedContextTurns() []protocol.Turn {
	c.mu.Lock()
	defer c.mu.Unlock()
	turns := make([]protocol.Turn, 0, len(c.turns))
	for _, turn := range c.turns {
		if _, omitted := c.omittedTurnIDs[turn.ID]; !omitted {
			turns = append(turns, turn)
		}
	}
	return cloneTurns(turns)
}
