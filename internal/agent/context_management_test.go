package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"Eylu/internal/config"
	contextledger "Eylu/internal/context"
	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/tool"
)

type twentyFileDriver struct {
	t           *testing.T
	requests    []driver.Request
	lastSummary string
}

func (d *twentyFileDriver) Name() string { return "twenty-files" }
func (d *twentyFileDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true}
}
func (d *twentyFileDriver) Generate(_ context.Context, request driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.assertAtomicToolPairs(request.Model.Turns)
	summary := turnTextByID(request.Model.Turns, "conversation-summary")
	if summary != "" && summary != d.lastSummary && len(request.Model.DriverState) != 0 {
		d.t.Fatalf("changed compression summary retained remote driver state: %s", request.Model.DriverState)
	}
	d.lastSummary = summary
	d.requests = append(d.requests, request)
	index := len(d.requests) - 1
	if index < 20 {
		arguments, _ := json.Marshal(map[string]string{"path": fmt.Sprintf("file-%02d.txt", index), "content": strings.Repeat(fmt.Sprintf("content-%02d ", index), 32)})
		call := protocol.ToolCall{ID: fmt.Sprintf("write-%02d", index), Name: "write_file", Arguments: arguments}
		return protocol.ModelResponse{
			Turn: protocol.Turn{ID: fmt.Sprintf("agent-%02d", index), Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartToolCall, ToolCall: &call}}},
			Stop: protocol.StopToolUse, Usage: protocol.Usage{InputTokens: 100, OutputTokens: 10, Exact: true}, DriverState: json.RawMessage(fmt.Sprintf(`{"response_id":"response-%02d"}`, index)),
		}, nil
	}
	return protocol.ModelResponse{Turn: protocol.Turn{ID: "agent-final", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "modified 20 files"}}}, Stop: protocol.StopCompleted, Usage: protocol.Usage{InputTokens: 100, OutputTokens: 10, Exact: true}}, nil
}

func turnTextByID(turns []protocol.Turn, id string) string {
	for _, turn := range turns {
		if turn.ID != id {
			continue
		}
		var text strings.Builder
		for _, part := range turn.Parts {
			text.WriteString(part.Text)
		}
		return text.String()
	}
	return ""
}

func (d *twentyFileDriver) assertAtomicToolPairs(turns []protocol.Turn) {
	calls := make(map[string]struct{})
	for _, turn := range turns {
		for _, part := range turn.Parts {
			if part.Kind == protocol.PartToolCall && part.ToolCall != nil {
				calls[part.ToolCall.ID] = struct{}{}
			}
		}
	}
	for _, turn := range turns {
		for _, part := range turn.Parts {
			if part.Kind == protocol.PartToolResult && part.ToolResult != nil {
				if _, ok := calls[part.ToolResult.CallID]; !ok {
					d.t.Fatalf("orphan tool result %s in request %#v", part.ToolResult.CallID, turns)
				}
			}
		}
	}
}

type contextCaptureDriver struct {
	request driver.Request
}

type reasoningPolicyDriver struct {
	t        *testing.T
	requests int
}

func (d *reasoningPolicyDriver) Name() string { return "reasoning-policy" }
func (d *reasoningPolicyDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{Reasoning: true}
}
func (d *reasoningPolicyDriver) Generate(_ context.Context, request driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.requests++
	if d.requests == 2 && strings.Contains(requestText(request.Model.Turns), "private-reasoning") {
		d.t.Fatal("reasoning content was replayed to the next model request")
	}
	return protocol.ModelResponse{Turn: protocol.Turn{ID: fmt.Sprintf("reasoning-%d", d.requests), Role: protocol.RoleAgent, Parts: []protocol.Part{
		{Kind: protocol.PartReasoning, Text: "private-reasoning"}, {Kind: protocol.PartText, Text: "public-answer"},
	}}, Stop: protocol.StopCompleted}, nil
}

func (d *contextCaptureDriver) Name() string                      { return "capture-context" }
func (d *contextCaptureDriver) Capabilities() driver.Capabilities { return driver.Capabilities{} }
func (d *contextCaptureDriver) Generate(_ context.Context, request driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.request = request
	return protocol.ModelResponse{Turn: protocol.Turn{ID: "captured", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "continued"}}}, Stop: protocol.StopCompleted}, nil
}

func TestTwentyFileConversationCompressesWithoutBreakingToolPairs(t *testing.T) {
	workspace := t.TempDir()
	writeFile, err := tool.NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	executor := &tool.Executor{Registry: tool.NewRegistry(writeFile), Policy: policy.AllowAllChecker{}, Workspace: workspace}
	model := &twentyFileDriver{t: t}
	events := make([]contextledger.Event, 0)
	runtime := compressionRuntime(model, workspace, func(event contextledger.Event) { events = append(events, event) })
	conversation := NewConversation()
	response, err := conversation.Run(context.Background(), "Create twenty files and remember goal-marker-20.", runtime, executor, LoopOptions{MaxTurns: 25, MaxTotalTokens: 100_000}, false, nil)
	if err != nil || response.Turn.Parts[0].Text != "modified 20 files" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	for index := 0; index < 20; index++ {
		if _, err := os.Stat(filepath.Join(workspace, fmt.Sprintf("file-%02d.txt", index))); err != nil {
			t.Fatalf("file %d: %v", index, err)
		}
	}
	if got := len(conversation.Transcript()); got != 42 {
		t.Fatalf("full transcript turns = %d, want 42", got)
	}
	if got := len(conversation.RetainedContextTurns()); got >= len(conversation.Transcript()) {
		t.Fatalf("retained=%d transcript=%d", got, len(conversation.Transcript()))
	}
	summary := conversation.ContextSummary()
	for _, expected := range []string{"User goals:", "goal-marker-20", "Completed modifications", "write_file file-00.txt", "Unfinished tasks:", "Validation results:"} {
		if !strings.Contains(summary, expected) {
			t.Fatalf("summary missing %q:\n%s", expected, summary)
		}
	}
	foundCompression, foundBudget := false, false
	for _, event := range events {
		foundCompression = foundCompression || event.Kind == contextledger.EventCompression
		foundBudget = foundBudget || event.Kind == contextledger.EventBudget
	}
	if !foundCompression || !foundBudget || conversation.ContextReport().CompressionCount == 0 {
		t.Fatalf("events=%#v report=%#v", events, conversation.ContextReport())
	}
	report := conversation.ContextReport()
	summedInput := 0
	foundProjectMap := false
	for _, category := range report.Categories {
		if category.Category != contextledger.CategoryOutputReserve {
			summedInput += category.Tokens
		}
		foundProjectMap = foundProjectMap || category.Category == contextledger.CategoryProjectContext && category.Tokens > 0
	}
	if summedInput != report.InputTokens || !foundProjectMap {
		t.Fatalf("context totals = %#v", report)
	}

	capture := &contextCaptureDriver{}
	second := compressionRuntime(capture, workspace, nil)
	second.Provider.Generation = 2
	if _, err := conversation.Send(context.Background(), "Continue with a portable summary.", second, false, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requestText(capture.request.Model.Turns), "goal-marker-20") || !strings.Contains(requestText(capture.request.Model.Turns), "write_file file-00.txt") || !strings.Contains(requestText(capture.request.Model.Turns), "file-19.txt") {
		t.Fatalf("portable request = %#v", capture.request.Model.Turns)
	}
}

func TestContextualizeTurnKeepsFullTranscriptAndLimitsModelResult(t *testing.T) {
	full := strings.Repeat("head-", 100) + "TAIL-MARKER"
	turn := protocol.Turn{ID: "tool", Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: &protocol.ToolResult{CallID: "call", Content: full}}}}
	contextTurn, keep := contextualizeTurn(turn, 120)
	if !keep || len(contextTurn.Parts[0].ToolResult.Content) > 120 || !strings.Contains(contextTurn.Parts[0].ToolResult.Content, "summarized") || !strings.Contains(contextTurn.Parts[0].ToolResult.Content, "TAIL-MARKER") {
		t.Fatalf("context turn = %#v", contextTurn)
	}
	if turn.Parts[0].ToolResult.Content != full {
		t.Fatal("full transcript tool result was mutated")
	}
}

func TestMCPContextIsClassifiedByServer(t *testing.T) {
	conversation := NewConversation()
	conversation.turns = []protocol.Turn{{
		ID: "tool-turn", Role: protocol.RoleTool,
		Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: &protocol.ToolResult{CallID: "mcp-call", Content: "mcp output", Metadata: map[string]any{"mcp_server": "fixture"}}}},
	}}
	mcpDefinition := protocol.ToolDefinition{Name: "mcp__fixture__echo", InputSchema: json.RawMessage(`{"type":"object"}`)}
	builtinDefinition := protocol.ToolDefinition{Name: "read_file", InputSchema: json.RawMessage(`{"type":"object"}`)}
	runtime := Runtime{
		MCPContexts:    []MCPContext{{Server: "fixture", Instructions: "server instructions", ResourceCatalog: `[{"uri":"fixture://resource"}]`}},
		MCPToolServers: map[string]string{"mcp__fixture__echo": "fixture"},
	}
	prepared := conversation.buildPromptContext(runtime, []protocol.ToolDefinition{builtinDefinition, mcpDefinition})
	wanted := map[contextledger.Category]string{
		contextledger.CategoryMCPInstructions: "fixture", contextledger.CategoryMCPResource: "fixture",
		contextledger.CategoryMCPToolSchema: "fixture:mcp__fixture__echo", contextledger.CategoryMCPToolResult: "fixture",
		contextledger.CategoryBuiltinToolSchema: "builtin:read_file",
	}
	for category, source := range wanted {
		found := false
		for _, block := range prepared.Blocks {
			if block.Category == category && block.Source == source {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing category=%s source=%s blocks=%#v", category, source, prepared.Blocks)
		}
	}
	if len(prepared.Tools) != 2 {
		t.Fatalf("tools = %#v", prepared.Tools)
	}
}

func TestMCPFingerprintChangeClearsDriverState(t *testing.T) {
	conversation := NewConversation()
	runtime := testRuntime(&contextCaptureDriver{}, 1)
	runtime.MCPFingerprint = "first"
	if err := conversation.prepareRuntime("prompt", runtime); err != nil {
		t.Fatal(err)
	}
	conversation.driverState = json.RawMessage(`{"remote":"state"}`)
	runtime.MCPFingerprint = "second"
	if err := conversation.prepareRuntime("prompt", runtime); err != nil {
		t.Fatal(err)
	}
	if len(conversation.driverState) != 0 {
		t.Fatalf("driver state = %s", conversation.driverState)
	}
}

func TestReasoningIsRetainedLocallyAndExcludedFromReplay(t *testing.T) {
	model := &reasoningPolicyDriver{t: t}
	conversation := NewConversation()
	runtime := testRuntime(model, 1)
	if _, err := conversation.Send(context.Background(), "first", runtime, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := conversation.Send(context.Background(), "second", runtime, false, nil); err != nil {
		t.Fatal(err)
	}
	transcript := conversation.Transcript()
	if transcript[1].Parts[0].Kind != protocol.PartReasoning || transcript[1].Parts[0].Text != "private-reasoning" {
		t.Fatalf("transcript = %#v", transcript)
	}
}

func compressionRuntime(model driver.ModelDriver, workspace string, event func(contextledger.Event)) Runtime {
	return Runtime{
		Provider: provider.Snapshot{Name: "work", Generation: 1, Config: config.ProviderConfig{
			Adapter: model.Name(), BaseURL: "https://example.com/v1", Model: "small-context", ContextWindow: 1800,
		}},
		Driver: model, Workspace: workspace, TokenEstimator: contextledger.ApproxEstimator{BytesPerToken: 2},
		OutputReserveTokens: 128, ContextRecentRounds: 2, MaxProjectMapBytes: 2048, MaxToolContextBytes: 512,
		SkillCatalogPageBytes: 512, MaxSummaryBytes: 4096, ContextEvent: event,
	}
}
