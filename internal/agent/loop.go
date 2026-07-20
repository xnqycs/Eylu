package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

type LoopOptions struct {
	MaxTurns       int
	MaxTotalTokens int
	RequestID      string
}

var ErrRequestInterrupted = errors.New("request interrupted by user")

func (c *Conversation) Run(ctx context.Context, prompt string, runtime Runtime, executor *tool.Executor, options LoopOptions, stream bool, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.prepareRuntime(prompt, runtime); err != nil {
		return protocol.ModelResponse{}, err
	}
	if executor == nil {
		return protocol.ModelResponse{}, fmt.Errorf("tool executor is nil")
	}
	maxTurns := options.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 20
	}
	c.appendUser(prompt)
	definitions := executor.Definitions()
	c.toolDefinitions = append(c.toolDefinitions[:0], definitions...)
	seenCalls := make(map[string]struct{})
	totalTokens := 0
	requestID := options.RequestID
	if requestID == "" {
		requestID = uuid.NewString()
	}
	var last protocol.ModelResponse
	for iteration := 0; iteration < maxTurns; iteration++ {
		if err := ctx.Err(); err != nil {
			return protocol.ModelResponse{}, err
		}
		parallelToolCalls := executor.ParallelLimit() > 1 && runtime.Driver.Capabilities().ParallelTools
		response, err := c.generate(ctx, runtime, definitions, parallelToolCalls, stream, emit)
		if err != nil {
			return protocol.ModelResponse{}, err
		}
		last = response
		totalTokens += response.Usage.InputTokens + response.Usage.OutputTokens
		if options.MaxTotalTokens > 0 && totalTokens > options.MaxTotalTokens {
			return last, &protocol.Error{Code: protocol.ErrProtocol, Message: "agent token budget exceeded"}
		}
		if response.Stop != protocol.StopToolUse {
			return response, nil
		}
		calls := toolCalls(response.Turn)
		if len(calls) == 0 {
			return last, &protocol.Error{Code: protocol.ErrProtocol, Message: "model stopped for tool use without tool calls"}
		}
		toolTurn := protocol.Turn{ID: uuid.NewString(), Role: protocol.RoleTool, CreatedAt: time.Now().UTC()}
		interrupted := false
		for _, call := range calls {
			if call.ID == "" {
				return last, &protocol.Error{Code: protocol.ErrProtocol, Message: "model returned a tool call without an ID"}
			}
			if _, duplicate := seenCalls[call.ID]; duplicate {
				return last, &protocol.Error{Code: protocol.ErrProtocol, Message: fmt.Sprintf("duplicate tool call ID %q", call.ID)}
			}
			seenCalls[call.ID] = struct{}{}
		}
		hooks := tool.BatchHooks{}
		if emit != nil {
			hooks.OnStart = func(call protocol.ToolCall) error {
				return emit(protocol.ModelEvent{Kind: protocol.EventToolStart, ToolCall: &call})
			}
			hooks.OnResult = func(result protocol.ToolResult) error {
				return emit(protocol.ModelEvent{Kind: protocol.EventToolResult, ToolResult: &result})
			}
		}
		results, batchErr := executor.ExecuteBatch(ctx, requestID, calls, hooks)
		for index := range results {
			result := results[index]
			if result.Metadata != nil && result.Metadata["interrupt_request"] == true {
				interrupted = true
			}
			c.captureSkillResult(result)
			c.captureTodoListResult(result)
			toolTurn.Parts = append(toolTurn.Parts, protocol.Part{Kind: protocol.PartToolResult, ToolResult: &result})
		}
		c.turns = append(c.turns, toolTurn)
		c.projectMapDirty = true
		if interrupted {
			c.driverState = nil
		}
		c.rebuildLedger(runtime)
		if batchErr != nil {
			return last, batchErr
		}
		if interrupted {
			return last, ErrRequestInterrupted
		}
	}
	return last, &protocol.Error{Code: protocol.ErrProtocol, Message: fmt.Sprintf("agent iteration limit exceeded (%d)", maxTurns)}
}

func (c *Conversation) captureTodoListResult(result protocol.ToolResult) {
	if result.IsError || result.TodoList == nil {
		return
	}
	c.todoList = cloneTodoList(*result.TodoList)
}

func toolCalls(turn protocol.Turn) []protocol.ToolCall {
	result := make([]protocol.ToolCall, 0)
	for _, part := range turn.Parts {
		if part.Kind == protocol.PartToolCall && part.ToolCall != nil {
			result = append(result, *part.ToolCall)
		}
	}
	return result
}
