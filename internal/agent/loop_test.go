package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
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
	executor := &tool.Executor{Registry: tool.NewRegistry(echoTool{}), Policy: policy.AllowAllChecker{}, Workspace: t.TempDir()}
	events := make([]protocol.EventKind, 0)
	response, err := conversation.Run(context.Background(), "use echo", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 3, MaxTotalTokens: 100}, false, func(event protocol.ModelEvent) error {
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
	if len(model.requests[0].Model.Tools) != 1 || len(model.requests[1].Model.Turns) != 4 {
		t.Fatalf("requests = %#v", model.requests)
	}
	if len(events) != 2 || events[0] != protocol.EventToolStart || events[1] != protocol.EventToolResult {
		t.Fatalf("events = %#v", events)
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

type concurrentLoopDriver struct{ requests atomic.Int32 }

func (d *concurrentLoopDriver) Name() string { return "concurrent-loop" }
func (d *concurrentLoopDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true, ParallelTools: true}
}
func (d *concurrentLoopDriver) Generate(_ context.Context, _ driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
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
	model := &concurrentLoopDriver{}
	parallelTool := &barrierReadTool{release: make(chan struct{})}
	executor := &tool.Executor{Registry: tool.NewRegistry(parallelTool), Policy: policy.AllowAllChecker{}, MaxParallelTools: 3, Timeout: time.Second}
	events := make([]protocol.ModelEvent, 0, 6)
	conversation := NewConversation()
	response, err := conversation.Run(context.Background(), "parallel reads", testRuntime(model, 1), executor, LoopOptions{MaxTurns: 3}, false, func(event protocol.ModelEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil || response.Turn.ID != "parallel-final" || parallelTool.ready.Load() != 3 {
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
		if events[index+3].Kind != protocol.EventToolResult || events[index+3].ToolResult.CallID != "call-"+expected {
			t.Fatalf("result event[%d] = %#v", index, events[index+3])
		}
	}
}
