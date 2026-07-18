package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

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
