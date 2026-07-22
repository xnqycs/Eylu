package openai_responses

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

type responseStreamEvent struct {
	Type        string       `json:"type"`
	Delta       string       `json:"delta"`
	Arguments   string       `json:"arguments"`
	OutputIndex int          `json:"output_index"`
	Item        responseItem `json:"item"`
	Response    responseBody `json:"response"`
	Error       *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type callAccumulator struct {
	ID        string
	Name      string
	Arguments strings.Builder
	Deltas    driver.StreamDeltaBuffer
}

func (d *Driver) readStream(ctx context.Context, body io.Reader, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	var data strings.Builder
	text := strings.Builder{}
	calls := make(map[int]*callAccumulator)
	startedWeb := make(map[string]bool)
	startedWebKinds := make(map[string]protocol.ToolKind)
	startedWebOrder := make([]string, 0)
	var final *protocol.ModelResponse
	completed := false
	emitToolDelta := func(index int, call *callAccumulator, delta string, done bool) error {
		if emit == nil || call == nil {
			return nil
		}
		update := &protocol.ToolCallDelta{OutputIndex: index, ID: call.ID, Name: call.Name, Delta: delta, Done: done}
		if done {
			update.Arguments = call.Arguments.String()
		}
		return emit(protocol.ModelEvent{Kind: protocol.EventToolCallDelta, ToolCallDelta: update})
	}
	dispatch := func() error {
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "" {
			return nil
		}
		if payload == "[DONE]" {
			completed = true
			return nil
		}
		var event responseStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return &protocol.Error{Code: protocol.ErrProtocol, Message: "decode Responses stream event", Cause: err}
		}
		switch event.Type {
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if emit != nil && event.Delta != "" {
				return emit(protocol.ModelEvent{Kind: protocol.EventReasoningDelta, Delta: event.Delta})
			}
		case "response.output_text.delta":
			text.WriteString(event.Delta)
			if emit != nil && event.Delta != "" {
				return emit(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: event.Delta})
			}
		case "response.output_item.added":
			if event.Item.Type == "function_call" {
				call := &callAccumulator{ID: event.Item.CallID, Name: event.Item.Name}
				if call.ID == "" {
					call.ID = event.Item.ID
				}
				call.Arguments.WriteString(event.Item.Arguments)
				calls[event.OutputIndex] = call
				if err := emitToolDelta(event.OutputIndex, call, "", false); err != nil {
					return err
				}
			} else if strings.Contains(event.Item.Type, "web_search_call") || strings.Contains(event.Item.Type, "web_fetch_call") {
				activity := webActivityFromItem(event.Item, nil)
				activity.Status = protocol.WebStatusRunning
				if !startedWeb[activity.CallID] {
					startedWebOrder = append(startedWebOrder, activity.CallID)
				}
				startedWeb[activity.CallID] = true
				startedWebKinds[activity.CallID] = activity.Kind
				kind := protocol.EventWebSearchStarted
				if activity.Kind == protocol.ToolWebFetch {
					kind = protocol.EventWebFetchStarted
				}
				if emit != nil {
					if err := emit(protocol.ModelEvent{Kind: kind, WebActivity: &activity}); err != nil {
						return err
					}
				}
			}
		case "response.function_call_arguments.delta":
			if call := calls[event.OutputIndex]; call != nil {
				call.Arguments.WriteString(event.Delta)
				if batch, ready := call.Deltas.Push(event.Delta, time.Now()); ready {
					if err := emitToolDelta(event.OutputIndex, call, batch, false); err != nil {
						return err
					}
				}
			}
		case "response.function_call_arguments.done":
			if call := calls[event.OutputIndex]; call != nil {
				if batch := call.Deltas.Flush(); batch != "" {
					if err := emitToolDelta(event.OutputIndex, call, batch, false); err != nil {
						return err
					}
				}
				arguments := event.Arguments
				if arguments == "" {
					arguments = event.Item.Arguments
				}
				if arguments != "" {
					call.Arguments.Reset()
					call.Arguments.WriteString(arguments)
				}
				if err := emitToolDelta(event.OutputIndex, call, "", true); err != nil {
					return err
				}
			}
		case "response.completed":
			converted := convertResponse(event.Response)
			finalText := responseText(converted)
			emittedText := text.String()
			missingText := ""
			if strings.HasPrefix(finalText, emittedText) {
				missingText = strings.TrimPrefix(finalText, emittedText)
			} else if emittedText == "" {
				missingText = finalText
			}
			if missingText != "" {
				text.WriteString(missingText)
				if emit != nil {
					if err := emit(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: missingText}); err != nil {
						return err
					}
				}
			}
			if emit != nil {
				if err := emitResponseParts(converted, emit, startedWeb, false); err != nil {
					return err
				}
			}
			final = &converted
			completed = true
		case "response.failed", "error":
			message := "Responses stream failed"
			if event.Error != nil && event.Error.Message != "" {
				message = event.Error.Message
			} else if event.Response.Error != nil && event.Response.Error.Message != "" {
				message = event.Response.Error.Message
			}
			if err := emitWebFailures(emit, startedWebKinds, startedWebOrder, message); err != nil {
				return err
			}
			return protocol.ClassifyProviderMessage(message)
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return protocol.ModelResponse{}, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if data.Len() > 0 {
		if err := dispatch(); err != nil {
			return protocol.ModelResponse{}, err
		}
	}
	if err := scanner.Err(); err != nil {
		_ = emitWebFailures(emit, startedWebKinds, startedWebOrder, err.Error())
		if ctx.Err() != nil {
			return protocol.ModelResponse{}, mapTransportError(ctx, ctx.Err())
		}
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrNetwork, Message: "read Responses stream", Retryable: true, Cause: err}
	}
	if !completed {
		_ = emitWebFailures(emit, startedWebKinds, startedWebOrder, "Responses stream ended before completion")
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrNetwork, Message: "Responses stream ended before completion", Retryable: true}
	}
	if final == nil {
		built := protocol.ModelResponse{Turn: protocol.Turn{ID: uuid.NewString(), Role: protocol.RoleAgent, CreatedAt: time.Now().UTC()}, Stop: protocol.StopCompleted}
		if text.Len() > 0 {
			built.Turn.Parts = append(built.Turn.Parts, protocol.Part{Kind: protocol.PartText, Text: text.String()})
		}
		for index := 0; index < len(calls); index++ {
			call := calls[index]
			if call == nil {
				continue
			}
			toolCall := protocol.ToolCall{ID: call.ID, Name: call.Name, Arguments: json.RawMessage(call.Arguments.String())}
			built.Turn.Parts = append(built.Turn.Parts, protocol.Part{Kind: protocol.PartToolCall, ToolCall: &toolCall})
			built.Stop = protocol.StopToolUse
		}
		final = &built
	}
	if len(final.Turn.Parts) == 0 {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "provider stream returned no text or tool calls"}
	}
	if emit != nil {
		if err := emit(protocol.ModelEvent{Kind: protocol.EventUsage, Usage: &final.Usage}); err != nil {
			return protocol.ModelResponse{}, err
		}
	}
	return *final, nil
}

func emitWebFailures(emit driver.EmitFunc, started map[string]protocol.ToolKind, order []string, message string) error {
	if emit == nil {
		return nil
	}
	for _, callID := range order {
		toolKind := started[callID]
		eventKind := protocol.EventWebSearchCompleted
		if toolKind == protocol.ToolWebFetch {
			eventKind = protocol.EventWebFetchCompleted
		}
		activity := protocol.WebActivity{CallID: callID, Kind: toolKind, Status: protocol.WebStatusError, Error: message}
		if err := emit(protocol.ModelEvent{Kind: eventKind, WebActivity: &activity}); err != nil {
			return err
		}
	}
	return nil
}

func responseText(response protocol.ModelResponse) string {
	var text strings.Builder
	for _, part := range response.Turn.Parts {
		if part.Kind == protocol.PartText {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}
