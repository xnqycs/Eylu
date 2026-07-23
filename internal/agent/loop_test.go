package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Eylu/internal/config"
	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/tool"
	"Eylu/internal/webtool"
)

type loopDriver struct {
	mu        sync.Mutex
	requests  []driver.Request
	always    bool
	duplicate bool
	parallel  bool
}

func (d *loopDriver) Name() string { return "loop" }
func (d *loopDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true}
}
func (d *loopDriver) Generate(_ context.Context, request driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, request)
	number := len(d.requests)
	if number == 1 || d.always || d.duplicate {
		id := fmt.Sprintf("call-%d", number)
		if d.duplicate {
			id = "duplicate"
		}
		call := protocol.ToolCall{ID: id, Name: "echo", Arguments: json.RawMessage(`{"value":"ok"}`)}
		parts := []protocol.Part{{Kind: protocol.PartToolCall, ToolCall: &call}}
		if d.parallel && number == 1 {
			missing := protocol.ToolCall{ID: "call-missing", Name: "missing", Arguments: json.RawMessage(`{}`)}
			parts = append(parts, protocol.Part{Kind: protocol.PartToolCall, ToolCall: &missing})
		}
		return protocol.ModelResponse{Turn: protocol.Turn{ID: fmt.Sprintf("agent-%d", number), Role: protocol.RoleAgent, Parts: parts}, Stop: protocol.StopToolUse, Usage: protocol.Usage{InputTokens: 5, OutputTokens: 1}}, nil
	}
	return protocol.ModelResponse{Turn: protocol.Turn{ID: "final", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "done"}}}, Stop: protocol.StopCompleted, Usage: protocol.Usage{InputTokens: 8, OutputTokens: 1}}, nil
}

func TestAgentLoopParallelCallsAndToolFailureContinue(t *testing.T) {
	model := &loopDriver{parallel: true}
	executor := &tool.Executor{Registry: tool.NewRegistry(echoTool{}), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	response, err := conversation.Run(context.Background(), "parallel", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 3, MaxTotalTokens: 100}, false, nil)
	if err != nil || response.Turn.Parts[0].Text != "done" {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
	turns := conversation.Transcript()
	if len(turns[2].Parts) != 2 || !turns[2].Parts[1].ToolResult.IsError || !strings.Contains(turns[2].Parts[1].ToolResult.Content, "unknown tool") {
		t.Fatalf("tool turn = %#v", turns[2])
	}
}

type echoTool struct{}

func (echoTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: "echo", Description: "echo", InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`)}
}
func (echoTool) Risk() policy.Risk { return policy.RiskRead }
func (echoTool) Execute(_ context.Context, input json.RawMessage) protocol.ToolResult {
	return protocol.ToolResult{Content: string(input)}
}

func TestAgentLoopTranscriptAndToolResultPairing(t *testing.T) {
	model := &loopDriver{}
	conversation := NewConversation()
	runtime := testRuntime(model, 1)
	runtime.Provider.Config.ReasoningEffort = "high"
	executor := &tool.Executor{Registry: tool.NewRegistry(echoTool{}), Policy: policy.AllowAllChecker{}, Workspace: t.TempDir()}
	events := make([]protocol.EventKind, 0)
	response, err := conversation.Run(context.Background(), "use echo", runtime, executor, LoopOptions{MaxTurns: 3, MaxTotalTokens: 100}, false, func(event protocol.ModelEvent) error {
		events = append(events, event.Kind)
		return nil
	})
	if err != nil || response.Turn.Parts[0].Text != "done" {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
	turns := conversation.Transcript()
	if len(turns) != 4 || turns[0].Role != protocol.RoleUser || turns[1].Role != protocol.RoleAgent || turns[2].Role != protocol.RoleTool || turns[3].Role != protocol.RoleAgent {
		t.Fatalf("turns = %#v", turns)
	}
	callID := turns[1].Parts[0].ToolCall.ID
	if turns[2].Parts[0].ToolResult.CallID != callID {
		t.Fatal("tool result is not paired with its call")
	}
	if len(model.requests[0].Model.Tools) != 1 || len(model.requests[1].Model.Turns) != 4 || model.requests[0].ReasoningEffort != "high" || model.requests[1].ReasoningEffort != "high" {
		t.Fatalf("requests = %#v", model.requests)
	}
	if len(events) != 2 || events[0] != protocol.EventToolStart || events[1] != protocol.EventToolResult {
		t.Fatalf("events = %#v", events)
	}
}

type approvalEchoTool struct{ calls int }

func (t *approvalEchoTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (t *approvalEchoTool) Risk() policy.Risk { return policy.RiskWrite }
func (t *approvalEchoTool) Execute(context.Context, json.RawMessage) protocol.ToolResult {
	t.calls++
	return protocol.ToolResult{Content: "executed"}
}

func TestAgentLoopInterruptsOnRejectionWithoutReason(t *testing.T) {
	model := &loopDriver{}
	item := &approvalEchoTool{}
	executor := &tool.Executor{
		Registry: tool.NewRegistry(item), Policy: policy.NewChecker(policy.DefaultConfig(policy.ModeManual)),
		Confirm: func(context.Context, policy.Request, policy.Outcome) (tool.Confirmation, error) {
			return tool.Confirmation{}, nil
		},
	}
	conversation := NewConversation()
	_, err := conversation.Run(context.Background(), "approval", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 3}, false, nil)
	if !errors.Is(err, ErrRequestInterrupted) || item.calls != 0 || len(model.requests) != 1 {
		t.Fatalf("error=%v calls=%d requests=%d", err, item.calls, len(model.requests))
	}
	turns := conversation.Transcript()
	if len(turns) != 3 || turns[2].Role != protocol.RoleTool || len(conversation.ExportState().DriverState) != 0 {
		t.Fatalf("turns=%#v state=%#v", turns, conversation.ExportState())
	}
}

func TestAgentLoopContinuesAfterRejectionWithReason(t *testing.T) {
	model := &loopDriver{}
	item := &approvalEchoTool{}
	executor := &tool.Executor{
		Registry: tool.NewRegistry(item), Policy: policy.NewChecker(policy.DefaultConfig(policy.ModeManual)),
		Confirm: func(context.Context, policy.Request, policy.Outcome) (tool.Confirmation, error) {
			return tool.Confirmation{RejectionReason: "Use the existing helper"}, nil
		},
	}
	conversation := NewConversation()
	response, err := conversation.Run(context.Background(), "approval", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 3}, false, nil)
	if err != nil || response.Turn.ID != "final" || item.calls != 0 || len(model.requests) != 2 {
		t.Fatalf("response=%#v error=%v calls=%d requests=%d", response, err, item.calls, len(model.requests))
	}
	if text := requestText(model.requests[1].Model.Turns); !strings.Contains(text, "Use the existing helper") {
		t.Fatalf("second request = %q", text)
	}
}

func TestAgentLoopLimitsAndDuplicateIDs(t *testing.T) {
	executor := &tool.Executor{Registry: tool.NewRegistry(echoTool{}), Policy: policy.AllowAllChecker{}}
	always := &loopDriver{always: true}
	_, err := NewConversation().Run(context.Background(), "loop", testRuntime(always, 1), executor, LoopOptions{MaxTurns: 2, MaxTotalTokens: 100}, false, nil)
	if typed, ok := err.(*protocol.Error); !ok || !strings.Contains(typed.Message, "iteration limit") {
		t.Fatalf("iteration error = %#v", err)
	}
	duplicate := &loopDriver{duplicate: true}
	_, err = NewConversation().Run(context.Background(), "duplicate", testRuntime(duplicate, 1), executor, LoopOptions{MaxTurns: 3, MaxTotalTokens: 100}, false, nil)
	if typed, ok := err.(*protocol.Error); !ok || !strings.Contains(typed.Message, "duplicate tool call ID") {
		t.Fatalf("duplicate error = %#v", err)
	}
}

type concurrentLoopDriver struct {
	requests           atomic.Int32
	parallelCapability bool
	lastRequest        driver.Request
}

func (d *concurrentLoopDriver) Name() string { return "concurrent-loop" }
func (d *concurrentLoopDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true, ParallelTools: d.parallelCapability}
}
func (d *concurrentLoopDriver) Generate(_ context.Context, request driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.lastRequest = request
	if d.requests.Add(1) == 1 {
		parts := make([]protocol.Part, 0, 3)
		for _, value := range []string{"first", "second", "third"} {
			call := protocol.ToolCall{ID: "call-" + value, Name: "parallel_read", Arguments: json.RawMessage(`{"value":"` + value + `"}`)}
			parts = append(parts, protocol.Part{Kind: protocol.PartToolCall, ToolCall: &call})
		}
		return protocol.ModelResponse{Turn: protocol.Turn{ID: "parallel-calls", Role: protocol.RoleAgent, Parts: parts}, Stop: protocol.StopToolUse}, nil
	}
	return protocol.ModelResponse{Turn: protocol.Turn{ID: "parallel-final", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "done"}}}, Stop: protocol.StopCompleted}, nil
}

type barrierReadTool struct {
	ready   atomic.Int32
	release chan struct{}
	once    sync.Once
}

func (t *barrierReadTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: "parallel_read", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (t *barrierReadTool) Risk() policy.Risk  { return policy.RiskRead }
func (t *barrierReadTool) ParallelSafe() bool { return true }
func (t *barrierReadTool) Execute(ctx context.Context, input json.RawMessage) protocol.ToolResult {
	if t.ready.Add(1) == 3 {
		t.once.Do(func() { close(t.release) })
	}
	select {
	case <-t.release:
	case <-ctx.Done():
		return protocol.ToolResult{Content: ctx.Err().Error(), IsError: true}
	}
	var parsed struct {
		Value string `json:"value"`
	}
	_ = json.Unmarshal(input, &parsed)
	result := protocol.ToolResult{Content: parsed.Value}
	if parsed.Value == "second" {
		result.IsError = true
	}
	return result
}

func TestAgentLoopRunsParallelSafeBatchAndOrdersResults(t *testing.T) {
	model := &concurrentLoopDriver{parallelCapability: true}
	parallelTool := &barrierReadTool{release: make(chan struct{})}
	executor := &tool.Executor{Registry: tool.NewRegistry(parallelTool), Policy: policy.AllowAllChecker{}, MaxParallelTools: 3, Timeout: time.Second}
	events := make([]protocol.ModelEvent, 0, 6)
	conversation := NewConversation()
	response, err := conversation.Run(context.Background(), "parallel reads", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 3}, false, func(event protocol.ModelEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil || response.Turn.ID != "parallel-final" || parallelTool.ready.Load() != 3 || !model.lastRequest.ParallelToolCalls {
		t.Fatalf("response=%#v ready=%d error=%v", response, parallelTool.ready.Load(), err)
	}
	toolTurn := conversation.Transcript()[2]
	if len(toolTurn.Parts) != 3 {
		t.Fatalf("tool turn = %#v", toolTurn)
	}
	for index, expected := range []string{"first", "second", "third"} {
		result := toolTurn.Parts[index].ToolResult
		if result.CallID != "call-"+expected || result.Content != expected || result.IsError != (expected == "second") {
			t.Fatalf("result[%d] = %#v", index, result)
		}
		if events[index].Kind != protocol.EventToolStart || events[index].ToolCall.ID != "call-"+expected {
			t.Fatalf("start event[%d] = %#v", index, events[index])
		}
	}
	completed := make(map[string]bool)
	for _, event := range events[3:] {
		if event.Kind != protocol.EventToolResult || event.ToolResult == nil {
			t.Fatalf("result event = %#v", event)
		}
		completed[event.ToolResult.CallID] = true
	}
	for _, expected := range []string{"call-first", "call-second", "call-third"} {
		if !completed[expected] {
			t.Fatalf("completion events = %#v", completed)
		}
	}
}

func TestAgentLoopRunsReturnedBatchWithoutDriverParallelCapability(t *testing.T) {
	model := &concurrentLoopDriver{}
	parallelTool := &barrierReadTool{release: make(chan struct{})}
	executor := &tool.Executor{Registry: tool.NewRegistry(parallelTool), Policy: policy.AllowAllChecker{}, MaxParallelTools: 3, Timeout: time.Second}
	response, err := NewConversation().Run(context.Background(), "parallel reads", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 3}, false, nil)
	if err != nil || response.Turn.ID != "parallel-final" || parallelTool.ready.Load() != 3 || model.lastRequest.ParallelToolCalls {
		t.Fatalf("response=%#v ready=%d request=%#v error=%v", response, parallelTool.ready.Load(), model.lastRequest, err)
	}
}

type namedMCPTool struct {
	name  string
	calls *atomic.Int32
}

func (t namedMCPTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: t.name, Description: t.name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (namedMCPTool) Risk() policy.Risk { return policy.RiskRead }
func (t namedMCPTool) Execute(context.Context, json.RawMessage) protocol.ToolResult {
	t.calls.Add(1)
	return protocol.ToolResult{Content: t.name + " executed"}
}

type changingMCPDriver struct {
	requests []driver.Request
	change   func()
}

func (*changingMCPDriver) Name() string { return "changing-mcp" }
func (*changingMCPDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true}
}
func (d *changingMCPDriver) Generate(_ context.Context, request driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.requests = append(d.requests, request)
	switch len(d.requests) {
	case 1:
		d.change()
		call := protocol.ToolCall{ID: "removed-call", Name: "mcp__fixture__removed", Arguments: json.RawMessage(`{}`)}
		return protocol.ModelResponse{Turn: protocol.Turn{ID: "removed", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartToolCall, ToolCall: &call}}}, Stop: protocol.StopToolUse}, nil
	case 2:
		call := protocol.ToolCall{ID: "added-call", Name: "mcp__fixture__added", Arguments: json.RawMessage(`{}`)}
		return protocol.ModelResponse{Turn: protocol.Turn{ID: "added", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartToolCall, ToolCall: &call}}}, Stop: protocol.StopToolUse}, nil
	default:
		return protocol.ModelResponse{Turn: protocol.Turn{ID: "complete", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "done"}}}, Stop: protocol.StopCompleted}, nil
	}
}

func TestAgentLoopAtomicallyRefreshesMCPToolsAndContext(t *testing.T) {
	var removedCalls, addedCalls atomic.Int32
	removed := namedMCPTool{name: "mcp__fixture__removed", calls: &removedCalls}
	added := namedMCPTool{name: "mcp__fixture__added", calls: &addedCalls}
	var stateMu sync.RWMutex
	state := MCPRuntimeState{
		Tools:       []tool.Tool{removed},
		Contexts:    []MCPContext{{Server: "fixture", Instructions: "old instructions", ResourceCatalog: "old resource"}},
		ToolServers: map[string]string{"mcp__fixture__removed": "fixture"},
		Fingerprint: "old-fingerprint",
	}
	driver := &changingMCPDriver{change: func() {
		stateMu.Lock()
		state = MCPRuntimeState{
			Tools:       []tool.Tool{added},
			Contexts:    []MCPContext{{Server: "fixture", Instructions: "new instructions", ResourceCatalog: "new resource"}},
			ToolServers: map[string]string{"mcp__fixture__added": "fixture"},
			Fingerprint: "new-fingerprint",
		}
		stateMu.Unlock()
	}}
	runtime := testRuntime(driver, 1)
	runtime.MCPContexts = []MCPContext{{Server: "fixture", Instructions: "old instructions", ResourceCatalog: "old resource"}}
	runtime.MCPToolServers = map[string]string{"mcp__fixture__removed": "fixture"}
	runtime.MCPFingerprint = "old-fingerprint"
	runtime.MCPState = func() MCPRuntimeState {
		stateMu.RLock()
		defer stateMu.RUnlock()
		return cloneMCPRuntimeState(state)
	}
	executor := &tool.Executor{Registry: tool.NewRegistry(echoTool{}, removed), Policy: policy.AllowAllChecker{}}
	conversation := NewConversation()
	response, err := conversation.Run(context.Background(), "refresh MCP", runtime, executor, LoopOptions{MaxTurns: 4}, false, nil)
	if err != nil || response.Turn.ID != "complete" {
		t.Fatalf("response=%#v error=%v", response, err)
	}
	if removedCalls.Load() != 0 || addedCalls.Load() != 1 {
		t.Fatalf("removed calls=%d added calls=%d", removedCalls.Load(), addedCalls.Load())
	}
	if names := definitionNames(driver.requests[0].Model.Tools); !strings.Contains(names, "mcp__fixture__removed") || strings.Contains(names, "mcp__fixture__added") {
		t.Fatalf("first definitions=%s", names)
	}
	if names := definitionNames(driver.requests[1].Model.Tools); strings.Contains(names, "mcp__fixture__removed") || !strings.Contains(names, "mcp__fixture__added") {
		t.Fatalf("second definitions=%s", names)
	}
	if text := requestText(driver.requests[1].Model.Turns); !strings.Contains(text, "new instructions") || !strings.Contains(text, "new resource") || !strings.Contains(text, "unknown tool") {
		t.Fatalf("refreshed request=%q", text)
	}
}

type hostedWebLoopDriver struct {
	requests         []driver.Request
	local            bool
	hostedActivities int
}

func (d *hostedWebLoopDriver) Name() string { return "hosted_web_loop" }
func (d *hostedWebLoopDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true, HostedWebSearch: true, HostedWebFetch: true, HostedAndFunctionTools: !d.local, SearchDomainFilter: true, SearchLocation: true}
}
func (d *hostedWebLoopDriver) Generate(_ context.Context, request driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.requests = append(d.requests, request)
	if d.local && len(d.requests) == 1 {
		call := protocol.ToolCall{ID: "web-1", Name: "web_search", Arguments: json.RawMessage(`{"query":"Eylu"}`)}
		return protocol.ModelResponse{Turn: protocol.Turn{Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartToolCall, ToolCall: &call}}}, Stop: protocol.StopToolUse}, nil
	}
	if d.local {
		return protocol.ModelResponse{Turn: protocol.Turn{Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "client done"}}}, Stop: protocol.StopCompleted}, nil
	}
	count := d.hostedActivities
	if count == 0 {
		count = 1
	}
	parts := make([]protocol.Part, 0, count+1)
	for index := 0; index < count; index++ {
		activity := protocol.WebActivity{CallID: fmt.Sprintf("hosted-%d", index+1), Kind: protocol.ToolWebSearch, Status: protocol.WebStatusCompleted}
		parts = append(parts, protocol.Part{Kind: protocol.PartWebActivity, WebActivity: &activity})
	}
	parts = append(parts, protocol.Part{Kind: protocol.PartText, Text: "hosted done"})
	return protocol.ModelResponse{Turn: protocol.Turn{Role: protocol.RoleAgent, Parts: parts}, Stop: protocol.StopCompleted}, nil
}

type webClientFixture struct{ calls atomic.Int32 }

func (t *webClientFixture) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: "mcp__web__search", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)}
}
func (t *webClientFixture) Risk() policy.Risk { return policy.RiskRead }
func (t *webClientFixture) Execute(_ context.Context, input json.RawMessage) protocol.ToolResult {
	t.calls.Add(1)
	return protocol.ToolResult{Content: string(input), Metadata: map[string]any{"mcp_server": "web"}}
}

func TestAgentLoopPublishesHostedWebToolsWithoutLocalExecution(t *testing.T) {
	model := &hostedWebLoopDriver{}
	runtime := Runtime{Provider: provider.Snapshot{Name: "work", Config: config.ProviderConfig{Adapter: model.Name(), Model: "model", WebTools: config.WebToolsConfig{Permission: config.WebPermissionAllow}}}, Driver: model, PermissionMode: "full"}
	executor := &tool.Executor{Registry: tool.NewRegistry(), Policy: policy.AllowAllChecker{}}
	response, err := NewConversation().Run(context.Background(), "search", runtime, executor, LoopOptions{MaxTurns: 2}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 1 || len(model.requests[0].Model.Tools) != 2 || model.requests[0].Model.Tools[0].Kind != protocol.ToolWebFetch || response.Turn.Parts[0].WebActivity == nil {
		t.Fatalf("request=%#v response=%#v", model.requests, response)
	}
}

func TestAgentLoopDefaultsHostedWebPermissionToAllow(t *testing.T) {
	model := &hostedWebLoopDriver{}
	runtime := Runtime{Provider: provider.Snapshot{Name: "work", Config: config.ProviderConfig{Adapter: model.Name(), Model: "model"}}, Driver: model, PermissionMode: "manual"}
	confirmations := 0
	executor := &tool.Executor{
		Registry: tool.NewRegistry(), Policy: policy.AllowAllChecker{},
		Confirm: func(context.Context, policy.Request, policy.Outcome) (tool.Confirmation, error) {
			confirmations++
			return tool.Confirmation{Approved: true}, nil
		},
	}
	if _, err := NewConversation().Run(context.Background(), "search", runtime, executor, LoopOptions{MaxTurns: 2}, false, nil); err != nil {
		t.Fatal(err)
	}
	if confirmations != 0 {
		t.Fatalf("hosted web requested %d confirmations with the default permission", confirmations)
	}
}

func TestAgentLoopExecutesClientWebFallbackThroughExistingTool(t *testing.T) {
	enabled := true
	target := &webClientFixture{}
	model := &hostedWebLoopDriver{local: true}
	runtime := Runtime{Provider: provider.Snapshot{Name: "work", Config: config.ProviderConfig{Adapter: model.Name(), Model: "model", WebTools: config.WebToolsConfig{
		Permission: config.WebPermissionAllow,
		Search:     config.WebToolConfig{Enabled: &enabled, Fallback: "client", ClientTool: target.Definition().Name},
	}}}, Driver: model, PermissionMode: "full"}
	executor := &tool.Executor{Registry: tool.NewRegistry(target), Policy: policy.AllowAllChecker{}}
	response, err := NewConversation().Run(context.Background(), "search", runtime, executor, LoopOptions{MaxTurns: 3}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Turn.Parts[0].Text != "client done" || target.calls.Load() != 1 || len(model.requests) != 2 || model.requests[0].Model.Tools[1].Execution != protocol.ExecutionClient {
		t.Fatalf("response=%#v calls=%d requests=%#v", response, target.calls.Load(), model.requests)
	}
}

func TestAgentLoopRebuildsStaleLocalWebFallback(t *testing.T) {
	enabled := true
	target := &webClientFixture{}
	model := &hostedWebLoopDriver{local: true}
	runtime := Runtime{Provider: provider.Snapshot{Name: "work", Config: config.ProviderConfig{Adapter: model.Name(), Model: "model", WebTools: config.WebToolsConfig{
		Permission: config.WebPermissionAllow,
		Search:     config.WebToolConfig{Enabled: &enabled, Fallback: "client", ClientTool: target.Definition().Name},
	}}}, Driver: model, PermissionMode: "full"}
	stale := webtool.NewLocalTool(webtool.ResolvedTool{
		Definition: protocol.ToolDefinition{
			Kind: protocol.ToolWebSearch, Name: "web_search", Execution: protocol.ExecutionClient,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
		},
		Execution: protocol.ExecutionClient, Target: target.Definition().Name,
	}, target, nil, config.WebPermissionAllow, webtool.NewUsageBudget())
	executor := &tool.Executor{Registry: tool.NewRegistry(target, stale), Policy: policy.AllowAllChecker{}}
	response, err := NewConversation().Run(context.Background(), "search", runtime, executor, LoopOptions{MaxTurns: 3}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Turn.Parts[0].Text != "client done" || target.calls.Load() != 1 {
		t.Fatalf("response=%#v calls=%d", response, target.calls.Load())
	}
}

type relayBatchWebDriver struct {
	requests atomic.Int32
}

func (*relayBatchWebDriver) Name() string { return "openai_responses" }
func (*relayBatchWebDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true, ParallelTools: true, HostedWebSearch: true, HostedAndFunctionTools: true}
}
func (d *relayBatchWebDriver) Generate(_ context.Context, _ driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	if d.requests.Add(1) == 1 {
		call := protocol.ToolCall{ID: "search-batch", Name: "web_search", Arguments: json.RawMessage(`{"queries":["one","two","three","four","five","six"]}`)}
		return protocol.ModelResponse{Turn: protocol.Turn{Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartToolCall, ToolCall: &call}}}, Stop: protocol.StopToolUse}, nil
	}
	return protocol.ModelResponse{Turn: protocol.Turn{Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "done"}}}, Stop: protocol.StopCompleted}, nil
}

func TestAgentLoopFansOutRelayBatchWebSearchConcurrentlyAndCollapsesResult(t *testing.T) {
	model := &relayBatchWebDriver{}
	var ready atomic.Int32
	gate := make(chan struct{})
	var once sync.Once
	delegate := func(ctx context.Context, resolved webtool.ResolvedTool, input json.RawMessage) protocol.ToolResult {
		var values struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(input, &values)
		if resolved.Target != "default" {
			return protocol.ToolResult{Content: "unexpected target " + resolved.Target, IsError: true}
		}
		if ready.Add(1) == 6 {
			once.Do(func() { close(gate) })
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return protocol.ToolResult{Content: ctx.Err().Error(), IsError: true}
		case <-time.After(time.Second):
			return protocol.ToolResult{Content: "parallel fan-out timed out", IsError: true}
		}
		activity := protocol.WebActivity{CallID: "provider-" + values.Query, Kind: protocol.ToolWebSearch, Query: values.Query, Action: "search", Status: protocol.WebStatusCompleted, Sources: []protocol.WebSource{{URL: "https://" + values.Query + ".example"}}}
		citation := protocol.URLCitation{CallID: activity.CallID, URL: activity.Sources[0].URL, Title: values.Query}
		structured, _ := json.Marshal(map[string]any{"activities": []protocol.WebActivity{activity}, "citations": []protocol.URLCitation{citation}})
		return protocol.ToolResult{Content: values.Query, StructuredContent: structured, Metadata: map[string]any{"web_backend": "delegated", "web_kind": "web_search"}}
	}
	runtime := Runtime{
		Provider: provider.Snapshot{Name: "default", Config: config.ProviderConfig{Adapter: "openai_responses", BaseURL: "https://relay.example/v1", Model: "gpt-5.6-sol", WebTools: config.WebToolsConfig{Permission: config.WebPermissionAllow}}},
		Driver:   model, PermissionMode: "full", WebDelegate: delegate,
	}
	executor := &tool.Executor{Registry: tool.NewRegistry(), Policy: policy.AllowAllChecker{}, MaxParallelTools: 2, Timeout: 2 * time.Second}
	events := make([]protocol.ModelEvent, 0, 18)
	conversation := NewConversation()
	response, err := conversation.Run(context.Background(), "parallel search", runtime, executor, LoopOptions{MaxTurns: 3}, false, func(event protocol.ModelEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil || response.Turn.Parts[0].Text != "done" || ready.Load() != 6 {
		t.Fatalf("response=%#v ready=%d err=%v", response, ready.Load(), err)
	}
	transcript := conversation.Transcript()
	toolTurn := transcript[2]
	results := make([]*protocol.ToolResult, 0)
	activities := make([]*protocol.WebActivity, 0)
	for index := range toolTurn.Parts {
		part := &toolTurn.Parts[index]
		if part.ToolResult != nil {
			results = append(results, part.ToolResult)
		}
		if part.WebActivity != nil {
			activities = append(activities, part.WebActivity)
		}
	}
	if len(results) != 1 || results[0].CallID != "search-batch" || results[0].IsError || len(activities) != 6 {
		t.Fatalf("tool turn = %#v", toolTurn)
	}
	for index, query := range []string{"one", "two", "three", "four", "five", "six"} {
		if !strings.Contains(results[0].Content, query) || activities[index].Query != query || activities[index].CallID != fmt.Sprintf("search-batch:%d", index+1) {
			t.Fatalf("query[%d]=%q result=%#v activity=%#v", index, query, results[0], activities[index])
		}
	}
	started := make(map[string]bool)
	completed := make(map[string]bool)
	for _, event := range events {
		if event.WebActivity == nil {
			continue
		}
		switch event.Kind {
		case protocol.EventWebSearchStarted:
			started[event.WebActivity.Query] = true
		case protocol.EventWebSearchCompleted:
			completed[event.WebActivity.Query] = true
		}
	}
	if len(started) != 6 || len(completed) != 6 {
		t.Fatalf("started=%#v completed=%#v events=%#v", started, completed, events)
	}
}

type webAuditCapture struct{ records []tool.AuditRecord }

func (capture *webAuditCapture) Record(record tool.AuditRecord) {
	capture.records = append(capture.records, record)
}

func TestAgentLoopApprovesAndAuditsHostedWebOncePerSubmission(t *testing.T) {
	model := &hostedWebLoopDriver{}
	runtime := Runtime{Provider: provider.Snapshot{Name: "work", Generation: 7, Config: config.ProviderConfig{Adapter: model.Name(), Model: "model", WebTools: config.WebToolsConfig{Permission: config.WebPermissionAsk}}}, Driver: model, PermissionMode: "manual"}
	audit := &webAuditCapture{}
	confirmations := 0
	executor := &tool.Executor{
		Registry: tool.NewRegistry(), Policy: policy.AllowAllChecker{}, Audit: audit,
		Confirm: func(_ context.Context, request policy.Request, outcome policy.Outcome) (tool.Confirmation, error) {
			confirmations++
			if request.Tool != "hosted_web" || request.Risk != policy.RiskNetwork || outcome.Decision != policy.DecisionConfirm {
				t.Fatalf("request=%#v outcome=%#v", request, outcome)
			}
			return tool.Confirmation{Approved: true}, nil
		},
	}
	if _, err := NewConversation().Run(context.Background(), "search", runtime, executor, LoopOptions{MaxTurns: 2, RequestID: "request-web"}, false, nil); err != nil {
		t.Fatal(err)
	}
	if confirmations != 1 || len(audit.records) != 1 || audit.records[0].WebBackend != "hosted" || audit.records[0].Risk != policy.RiskNetwork || !audit.records[0].UntrustedWebContent {
		t.Fatalf("confirmations=%d audit=%#v", confirmations, audit.records)
	}
}

func TestAgentLoopRejectsHostedWebActivityBeyondMaxUses(t *testing.T) {
	enabled := true
	model := &hostedWebLoopDriver{hostedActivities: 2}
	runtime := Runtime{Provider: provider.Snapshot{Name: "work", Config: config.ProviderConfig{Adapter: model.Name(), Model: "model", WebTools: config.WebToolsConfig{
		Permission: config.WebPermissionAllow, Search: config.WebToolConfig{Enabled: &enabled, MaxUses: 1},
	}}}, Driver: model, PermissionMode: "full"}
	_, err := NewConversation().Run(context.Background(), "search", runtime, &tool.Executor{Registry: tool.NewRegistry(), Policy: policy.AllowAllChecker{}}, LoopOptions{MaxTurns: 2}, false, nil)
	var typed *protocol.Error
	if !errors.As(err, &typed) || typed.Code != protocol.ErrTool || !strings.Contains(typed.Message, "max_uses") {
		t.Fatalf("err=%v", err)
	}
}

func definitionNames(definitions []protocol.ToolDefinition) string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	return strings.Join(names, ",")
}
