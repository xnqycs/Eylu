package agent

import (
	"context"
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
}

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
	requestID := uuid.NewString()
	var last protocol.ModelResponse
	for iteration := 0; iteration < maxTurns; iteration++ {
		if err := ctx.Err(); err != nil {
			return protocol.ModelResponse{}, err
		}
		response, err := c.generate(ctx, runtime, definitions, stream, emit)
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
		for _, call := range calls {
			if call.ID == "" {
				return last, &protocol.Error{Code: protocol.ErrProtocol, Message: "model returned a tool call without an ID"}
			}
			if _, duplicate := seenCalls[call.ID]; duplicate {
				return last, &protocol.Error{Code: protocol.ErrProtocol, Message: fmt.Sprintf("duplicate tool call ID %q", call.ID)}
			}
			seenCalls[call.ID] = struct{}{}
			if emit != nil {
				if err := emit(protocol.ModelEvent{Kind: protocol.EventToolStart, ToolCall: &call}); err != nil {
					return last, err
				}
			}
			result := executor.Execute(ctx, requestID, call)
			c.captureSkillResult(result)
			toolTurn.Parts = append(toolTurn.Parts, protocol.Part{Kind: protocol.PartToolResult, ToolResult: &result})
			if emit != nil {
				if err := emit(protocol.ModelEvent{Kind: protocol.EventToolResult, ToolResult: &result}); err != nil {
					return last, err
				}
			}
		}
		c.turns = append(c.turns, toolTurn)
		c.rebuildLedger(runtime)
	}
	return last, &protocol.Error{Code: protocol.ErrProtocol, Message: fmt.Sprintf("agent iteration limit exceeded (%d)", maxTurns)}
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
