package tool

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type cancellingParallelTool struct {
	active  atomic.Int32
	started atomic.Int32
}

func (t *cancellingParallelTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: "wait", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (t *cancellingParallelTool) Risk() policy.Risk  { return policy.RiskRead }
func (t *cancellingParallelTool) ParallelSafe() bool { return true }
func (t *cancellingParallelTool) Execute(ctx context.Context, _ json.RawMessage) protocol.ToolResult {
	t.started.Add(1)
	t.active.Add(1)
	defer t.active.Add(-1)
	<-ctx.Done()
	return protocol.ToolResult{Content: ctx.Err().Error(), IsError: true}
}

func TestExecuteConcurrentCancellationConverges(t *testing.T) {
	parallelTool := &cancellingParallelTool{}
	executor := &Executor{Registry: NewRegistry(parallelTool), Policy: policy.AllowAllChecker{}, MaxParallelTools: 2, Timeout: time.Second}
	calls := make([]protocol.ToolCall, 5)
	for index := range calls {
		calls[index] = protocol.ToolCall{ID: string(rune('a' + index)), Name: "wait", Arguments: json.RawMessage(`{}`)}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan []protocol.ToolResult, 1)
	go func() { done <- executor.ExecuteConcurrent(ctx, "request", calls) }()
	deadline := time.After(time.Second)
	for parallelTool.active.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("parallel tools did not start")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case results := <-done:
		if len(results) != len(calls) || parallelTool.active.Load() != 0 || parallelTool.started.Load() != 2 {
			t.Fatalf("results=%d active=%d started=%d", len(results), parallelTool.active.Load(), parallelTool.started.Load())
		}
		for index, result := range results {
			if result.CallID != calls[index].ID || !result.IsError || !strings.Contains(result.Content, "cancel") {
				t.Fatalf("result[%d] = %#v", index, result)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("parallel cancellation did not converge")
	}
}

type panickingParallelTool struct{}

func (panickingParallelTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: "panic", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (panickingParallelTool) Risk() policy.Risk  { return policy.RiskRead }
func (panickingParallelTool) ParallelSafe() bool { return true }
func (panickingParallelTool) Execute(context.Context, json.RawMessage) protocol.ToolResult {
	panic("fixture panic")
}

func TestExecuteRecoversToolPanic(t *testing.T) {
	executor := &Executor{Registry: NewRegistry(panickingParallelTool{}), Policy: policy.AllowAllChecker{}}
	result := executor.Execute(context.Background(), "request", protocol.ToolCall{ID: "panic-call", Name: "panic", Arguments: json.RawMessage(`{}`)})
	if !result.IsError || result.CallID != "panic-call" || !strings.Contains(result.Content, "fixture panic") {
		t.Fatalf("result = %#v", result)
	}
}
