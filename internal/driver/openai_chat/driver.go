package openai_chat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

const Name = "openai_chat"

type Driver struct {
	client *http.Client
}

func New(client *http.Client) *Driver {
	if client == nil {
		client = http.DefaultClient
	}
	return &Driver{client: client}
}

func (d *Driver) Name() string { return Name }

func (d *Driver) Capabilities() driver.Capabilities {
	return driver.Capabilities{TextStreaming: true, ToolCalling: true, ParallelTools: true, ImageInput: true}
}

type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Tools         []chatTool    `json:"tools,omitempty"`
	Stream        bool          `json:"stream,omitempty"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

type chatMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   string          `json:"arguments,omitempty"`
}

type chatToolCall struct {
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message      chatMessage `json:"message"`
		Delta        chatMessage `json:"delta"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		CompletionDetail struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (d *Driver) Generate(ctx context.Context, req driver.Request, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	body := chatRequest{Model: req.Model.Model, Stream: req.Stream}
	if req.Stream {
		body.StreamOptions = &struct {
			IncludeUsage bool `json:"include_usage"`
		}{IncludeUsage: true}
	}
	for _, turn := range req.Model.Turns {
		message := chatMessage{Role: chatRole(turn.Role)}
		for _, part := range turn.Parts {
			switch {
			case part.Kind == protocol.PartText:
				message.Content += part.Text
			case part.Kind == protocol.PartToolCall && part.ToolCall != nil:
				message.ToolCalls = append(message.ToolCalls, chatToolCall{ID: part.ToolCall.ID, Type: "function", Function: chatFunction{Name: part.ToolCall.Name, Arguments: string(part.ToolCall.Arguments)}})
			case part.Kind == protocol.PartToolResult && part.ToolResult != nil:
				body.Messages = append(body.Messages, chatMessage{Role: "tool", ToolCallID: part.ToolResult.CallID, Content: part.ToolResult.Content})
			}
		}
		if message.Content != "" || len(message.ToolCalls) > 0 {
			body.Messages = append(body.Messages, message)
		}
	}
	for _, definition := range req.Model.Tools {
		body.Tools = append(body.Tools, chatTool{Type: "function", Function: chatFunction{Name: definition.Name, Description: definition.Description, Parameters: definition.InputSchema}})
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "encode chat request", Cause: err}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(req.BaseURL, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrConfig, Message: "build chat request", Cause: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	for key, value := range req.Headers {
		httpReq.Header.Set(key, value)
	}
	if emit != nil {
		if err := emit(protocol.ModelEvent{Kind: protocol.EventResponseStart}); err != nil {
			return protocol.ModelResponse{}, err
		}
	}
	client := d.client
	if req.Stream {
		client = driver.StreamingHTTPClient(client)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return protocol.ModelResponse{}, mapTransportError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return protocol.ModelResponse{}, mapHTTPError(resp.StatusCode, raw)
	}
	if req.Stream {
		return d.readStream(ctx, resp.Body, emit)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrNetwork, Message: "read chat response", Retryable: true, Cause: err}
	}
	var decoded chatResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "decode chat response", Cause: err}
	}
	if decoded.Error != nil {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProvider, Message: decoded.Error.Message}
	}
	result := convertResponse(decoded)
	if len(result.Turn.Parts) == 0 {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "provider returned no text or tool calls"}
	}
	if emit != nil {
		for _, part := range result.Turn.Parts {
			if part.Kind == protocol.PartText {
				if err := emit(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: part.Text}); err != nil {
					return protocol.ModelResponse{}, err
				}
			}
		}
		if err := emit(protocol.ModelEvent{Kind: protocol.EventUsage, Usage: &result.Usage}); err != nil {
			return protocol.ModelResponse{}, err
		}
		if err := emit(protocol.ModelEvent{Kind: protocol.EventResponseDone, Response: &result}); err != nil {
			return protocol.ModelResponse{}, err
		}
	}
	return result, nil
}

func (d *Driver) readStream(ctx context.Context, body io.Reader, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	text := strings.Builder{}
	calls := make(map[int]*chatToolCall)
	deltaBuffers := make(map[int]*driver.StreamDeltaBuffer)
	result := protocol.ModelResponse{Turn: protocol.Turn{ID: uuid.NewString(), Role: protocol.RoleAgent, CreatedAt: time.Now().UTC()}, Stop: protocol.StopCompleted}
	completed := false
	emitToolDelta := func(index int, call *chatToolCall, delta string, done bool) error {
		if emit == nil || call == nil {
			return nil
		}
		update := &protocol.ToolCallDelta{OutputIndex: index, ID: call.ID, Name: call.Function.Name, Delta: delta, Done: done}
		if done {
			update.Arguments = call.Function.Arguments
		}
		return emit(protocol.ModelEvent{Kind: protocol.EventToolCallDelta, ToolCallDelta: update})
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			for index := 0; index < len(calls); index++ {
				if buffer := deltaBuffers[index]; buffer != nil {
					if batch := buffer.Flush(); batch != "" {
						if err := emitToolDelta(index, calls[index], batch, false); err != nil {
							return protocol.ModelResponse{}, err
						}
					}
				}
				if err := emitToolDelta(index, calls[index], "", true); err != nil {
					return protocol.ModelResponse{}, err
				}
			}
			completed = true
			continue
		}
		var chunk chatResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "decode chat stream chunk", Cause: err}
		}
		if chunk.Error != nil {
			return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProvider, Message: chunk.Error.Message}
		}
		if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
			result.Usage = usageFromChat(chunk)
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.ReasoningContent != "" && emit != nil {
				if err := emit(protocol.ModelEvent{Kind: protocol.EventReasoningDelta, Delta: choice.Delta.ReasoningContent}); err != nil {
					return protocol.ModelResponse{}, err
				}
			}
			if choice.Delta.Content != "" {
				text.WriteString(choice.Delta.Content)
				if emit != nil {
					if err := emit(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: choice.Delta.Content}); err != nil {
						return protocol.ModelResponse{}, err
					}
				}
			}
			for _, delta := range choice.Delta.ToolCalls {
				call := calls[delta.Index]
				created := call == nil
				if call == nil {
					copy := delta
					call = &copy
					calls[delta.Index] = call
					deltaBuffers[delta.Index] = &driver.StreamDeltaBuffer{}
				} else {
					if delta.ID != "" {
						call.ID = delta.ID
					}
					if delta.Function.Name != "" {
						call.Function.Name = delta.Function.Name
					}
					call.Function.Arguments += delta.Function.Arguments
				}
				if created {
					if err := emitToolDelta(delta.Index, call, "", false); err != nil {
						return protocol.ModelResponse{}, err
					}
				}
				if batch, ready := deltaBuffers[delta.Index].Push(delta.Function.Arguments, time.Now()); ready {
					if err := emitToolDelta(delta.Index, call, batch, false); err != nil {
						return protocol.ModelResponse{}, err
					}
				}
			}
			result.Stop = stopFromFinishReason(choice.FinishReason)
		}
	}
	if err := scanner.Err(); err != nil {
		return protocol.ModelResponse{}, mapTransportError(ctx, err)
	}
	if !completed {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrNetwork, Message: "chat stream ended before completion", Retryable: true}
	}
	if text.Len() > 0 {
		result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartText, Text: text.String()})
	}
	for index := 0; index < len(calls); index++ {
		call := calls[index]
		if call == nil {
			continue
		}
		toolCall := protocol.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: json.RawMessage(call.Function.Arguments)}
		result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartToolCall, ToolCall: &toolCall})
		result.Stop = protocol.StopToolUse
	}
	if len(result.Turn.Parts) == 0 {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "provider stream returned no text or tool calls"}
	}
	if emit != nil {
		if err := emit(protocol.ModelEvent{Kind: protocol.EventUsage, Usage: &result.Usage}); err != nil {
			return protocol.ModelResponse{}, err
		}
		if err := emit(protocol.ModelEvent{Kind: protocol.EventResponseDone, Response: &result}); err != nil {
			return protocol.ModelResponse{}, err
		}
	}
	return result, nil
}

func convertResponse(decoded chatResponse) protocol.ModelResponse {
	result := protocol.ModelResponse{Turn: protocol.Turn{ID: uuid.NewString(), Role: protocol.RoleAgent, CreatedAt: time.Now().UTC()}, Stop: protocol.StopCompleted, Usage: usageFromChat(decoded)}
	if len(decoded.Choices) == 0 {
		return result
	}
	choice := decoded.Choices[0]
	if choice.Message.Content != "" {
		result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartText, Text: choice.Message.Content})
	}
	for _, item := range choice.Message.ToolCalls {
		call := protocol.ToolCall{ID: item.ID, Name: item.Function.Name, Arguments: json.RawMessage(item.Function.Arguments)}
		result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartToolCall, ToolCall: &call})
	}
	result.Stop = stopFromFinishReason(choice.FinishReason)
	return result
}

func usageFromChat(decoded chatResponse) protocol.Usage {
	return protocol.Usage{InputTokens: decoded.Usage.PromptTokens, OutputTokens: decoded.Usage.CompletionTokens, ReasoningTokens: decoded.Usage.CompletionDetail.ReasoningTokens, Exact: true}
}

func stopFromFinishReason(reason string) protocol.StopKind {
	switch reason {
	case "tool_calls", "function_call":
		return protocol.StopToolUse
	case "length":
		return protocol.StopLength
	default:
		return protocol.StopCompleted
	}
}

func chatRole(role protocol.Role) string {
	switch role {
	case protocol.RoleAgent:
		return "assistant"
	default:
		return string(role)
	}
}

func mapTransportError(ctx context.Context, err error) error {
	if ctx.Err() == context.Canceled {
		return &protocol.Error{Code: protocol.ErrCancelled, Message: "chat request cancelled", Cause: ctx.Err()}
	}
	if ctx.Err() == context.DeadlineExceeded {
		return &protocol.Error{Code: protocol.ErrTimeout, Message: "chat request timed out", Retryable: true, Cause: ctx.Err()}
	}
	return &protocol.Error{Code: protocol.ErrNetwork, Message: "chat request failed", Retryable: true, Cause: err}
}

func mapHTTPError(status int, body []byte) error {
	message := strings.TrimSpace(string(body))
	var envelope chatResponse
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != nil && envelope.Error.Message != "" {
		message = envelope.Error.Message
	}
	if len(message) > 512 {
		message = message[:512]
	}
	code, retryable := protocol.ErrProvider, status >= 500
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		code = protocol.ErrAuth
	}
	if status == http.StatusTooManyRequests {
		code, retryable = protocol.ErrRateLimit, true
	}
	return &protocol.Error{Code: code, Message: fmt.Sprintf("provider HTTP %d: %s", status, message), StatusCode: status, Retryable: retryable}
}
