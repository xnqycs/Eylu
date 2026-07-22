package openai_chat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

const Name = "openai_chat"

type Driver struct {
	client              *http.Client
	parallelUnsupported sync.Map
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

func (d *Driver) CapabilitiesFor(target driver.CapabilityTarget) driver.Capabilities {
	capabilities := d.Capabilities()
	capabilities.HostedToolStreaming, capabilities.HostedAndFunctionTools, capabilities.SearchUsageDetails = true, true, true
	switch strings.ToLower(strings.TrimSpace(target.Provider)) {
	case "groq":
		capabilities.HostedWebSearch, capabilities.HostedWebFetch = true, true
		capabilities.HostedAndFunctionTools = false
	case "qwen", "dashscope", "alibaba":
		capabilities.HostedWebSearch = true
	case "openrouter":
		capabilities.HostedWebSearch, capabilities.HostedWebFetch = true, true
		capabilities.SearchDomainFilter = true
	case "openai", "xai":
		capabilities.HostedWebSearch = true
		capabilities.SearchDomainFilter, capabilities.SearchLocation = true, true
	}
	return capabilities
}

type chatRequest struct {
	Model             string        `json:"model"`
	Messages          []chatMessage `json:"messages"`
	Tools             []chatTool    `json:"tools,omitempty"`
	ReasoningEffort   string        `json:"reasoning_effort,omitempty"`
	ParallelToolCalls bool          `json:"parallel_tool_calls,omitempty"`
	Stream            bool          `json:"stream,omitempty"`
	StreamOptions     *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	WebSearchOptions map[string]any  `json:"web_search_options,omitempty"`
	EnableSearch     bool            `json:"enable_search,omitempty"`
	SearchOptions    map[string]any  `json:"search_options,omitempty"`
	CompoundCustom   *compoundCustom `json:"compound_custom,omitempty"`
}

type compoundCustom struct {
	Tools struct {
		EnabledTools []string `json:"enabled_tools"`
	} `json:"tools"`
}

type chatMessage struct {
	Role             string           `json:"role"`
	Content          string           `json:"content,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall   `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	Annotations      []chatAnnotation `json:"annotations,omitempty"`
}

type chatAnnotation struct {
	Type       string `json:"type"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
}

type chatTool struct {
	Type     string        `json:"type"`
	Function *chatFunction `json:"function,omitempty"`
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
	Citations []string `json:"citations"`
}

func (d *Driver) Generate(ctx context.Context, req driver.Request, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	body := chatRequest{Model: req.Model.Model, Stream: req.Stream}
	parallelKey := requestCapabilityKey(req)
	if req.ParallelToolCalls && len(req.Model.Tools) > 0 {
		_, unsupported := d.parallelUnsupported.Load(parallelKey)
		body.ParallelToolCalls = !unsupported
	}
	effort := strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	if effort != "auto" {
		body.ReasoningEffort = effort
	}
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
				body.Messages = append(body.Messages, chatMessage{Role: "tool", ToolCallID: part.ToolResult.CallID, Content: driver.ToolResultContent(*part.ToolResult)})
			}
		}
		if message.Content != "" || len(message.ToolCalls) > 0 {
			body.Messages = append(body.Messages, message)
		}
	}
	hostedKind, hasHosted, err := mapChatTools(&body, req.Model.Tools, req.Target)
	if err != nil {
		return protocol.ModelResponse{}, err
	}
	resp, err := d.send(ctx, req, body)
	if err != nil {
		return protocol.ModelResponse{}, mapSendError(ctx, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if body.ParallelToolCalls && parallelToolCallsUnsupported(resp.StatusCode, raw) {
			d.parallelUnsupported.Store(parallelKey, struct{}{})
			body.ParallelToolCalls = false
			resp, err = d.send(ctx, req, body)
			if err != nil {
				return protocol.ModelResponse{}, mapSendError(ctx, err)
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				raw, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				return protocol.ModelResponse{}, mapHTTPErrorWithTools(resp.StatusCode, raw, hasHosted)
			}
		} else {
			return protocol.ModelResponse{}, mapHTTPErrorWithTools(resp.StatusCode, raw, hasHosted)
		}
	}
	defer resp.Body.Close()
	if emit != nil {
		if err := emit(protocol.ModelEvent{Kind: protocol.EventResponseStart}); err != nil {
			return protocol.ModelResponse{}, err
		}
	}
	if req.Stream {
		return d.readStream(ctx, resp.Body, emit, hostedKind)
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
		return protocol.ModelResponse{}, protocol.ClassifyProviderMessage(decoded.Error.Message)
	}
	result := convertResponse(decoded, hostedKind, raw)
	if len(result.Turn.Parts) == 0 {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "provider returned no text or tool calls"}
	}
	if emit != nil {
		if err := emitChatParts(result, emit, true); err != nil {
			return protocol.ModelResponse{}, err
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

func (d *Driver) send(ctx context.Context, req driver.Request, body chatRequest) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrProtocol, Message: "encode chat request", Cause: err}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(req.BaseURL, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrConfig, Message: "build chat request", Cause: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	for key, value := range req.Headers {
		httpReq.Header.Set(key, value)
	}
	client := d.client
	if req.Stream {
		client = driver.StreamingHTTPClient(client)
	}
	return client.Do(httpReq)
}

func mapSendError(ctx context.Context, err error) error {
	var protocolError *protocol.Error
	if errors.As(err, &protocolError) {
		return err
	}
	return mapTransportError(ctx, err)
}

func requestCapabilityKey(req driver.Request) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(req.BaseURL)), "/") + "\x00" + strings.ToLower(strings.TrimSpace(req.Model.Model))
}

func parallelToolCallsUnsupported(status int, raw []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	message := strings.ToLower(string(raw))
	if !strings.Contains(message, "parallel_tool_calls") {
		return false
	}
	for _, marker := range []string{"unsupported", "unknown", "unrecognized", "invalid", "unexpected", "not permitted", "not allowed"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func (d *Driver) readStream(ctx context.Context, body io.Reader, emit driver.EmitFunc, hostedKind protocol.ToolKind) (protocol.ModelResponse, error) {
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
			return protocol.ModelResponse{}, protocol.ClassifyProviderMessage(chunk.Error.Message)
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
			for _, annotation := range choice.Delta.Annotations {
				if annotation.Type == "url_citation" && annotation.URL != "" {
					citation := protocol.URLCitation{URL: annotation.URL, Title: annotation.Title, StartIndex: annotation.StartIndex, EndIndex: annotation.EndIndex}
					result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartCitation, Citation: &citation})
					if emit != nil {
						if err := emit(protocol.ModelEvent{Kind: protocol.EventCitation, Citation: &citation}); err != nil {
							return protocol.ModelResponse{}, err
						}
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
		result.Turn.Parts = append([]protocol.Part{{Kind: protocol.PartText, Text: text.String()}}, result.Turn.Parts...)
	}
	if hostedKind.IsWeb() && len(result.Turn.Parts) > 0 {
		activity := protocol.WebActivity{CallID: "web-stream", Kind: hostedKind, Status: protocol.WebStatusCompleted, Usage: webUsage(hostedKind)}
		result.Turn.Parts = append([]protocol.Part{{Kind: protocol.PartWebActivity, WebActivity: &activity}}, result.Turn.Parts...)
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

func convertResponse(decoded chatResponse, hostedKind protocol.ToolKind, raw []byte) protocol.ModelResponse {
	result := protocol.ModelResponse{Turn: protocol.Turn{ID: uuid.NewString(), Role: protocol.RoleAgent, CreatedAt: time.Now().UTC()}, Stop: protocol.StopCompleted, Usage: usageFromChat(decoded)}
	if len(decoded.Choices) == 0 {
		return result
	}
	choice := decoded.Choices[0]
	callID := ""
	if hostedKind.IsWeb() && (len(choice.Message.Annotations) > 0 || len(decoded.Citations) > 0) {
		callID = "web-" + decoded.ID
		if callID == "web-" {
			callID = uuid.NewString()
		}
		activity := protocol.WebActivity{CallID: callID, Kind: hostedKind, Status: protocol.WebStatusCompleted, Usage: webUsage(hostedKind), RawProviderResponse: boundedRaw(raw), RawTruncated: len(raw) > 256<<10}
		result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartWebActivity, WebActivity: &activity})
	}
	if choice.Message.Content != "" {
		result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartText, Text: choice.Message.Content})
	}
	seenCitations := make(map[string]bool)
	for _, annotation := range choice.Message.Annotations {
		if annotation.Type != "url_citation" || annotation.URL == "" || seenCitations[annotation.URL] {
			continue
		}
		seenCitations[annotation.URL] = true
		citation := protocol.URLCitation{CallID: callID, URL: annotation.URL, Title: annotation.Title, StartIndex: annotation.StartIndex, EndIndex: annotation.EndIndex}
		result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartCitation, Citation: &citation})
	}
	for _, url := range decoded.Citations {
		if url == "" || seenCitations[url] {
			continue
		}
		seenCitations[url] = true
		citation := protocol.URLCitation{CallID: callID, URL: url}
		result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartCitation, Citation: &citation})
	}
	for _, item := range choice.Message.ToolCalls {
		call := protocol.ToolCall{ID: item.ID, Name: item.Function.Name, Arguments: json.RawMessage(item.Function.Arguments)}
		result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartToolCall, ToolCall: &call})
	}
	result.Stop = stopFromFinishReason(choice.FinishReason)
	return result
}

func mapChatTools(body *chatRequest, definitions []protocol.ToolDefinition, target driver.CapabilityTarget) (protocol.ToolKind, bool, error) {
	provider := strings.ToLower(strings.TrimSpace(target.Provider))
	var hostedKind protocol.ToolKind
	for _, definition := range definitions {
		kind := definition.Kind.Effective()
		execution := definition.Execution.Effective()
		if kind == protocol.ToolFunction || execution == protocol.ExecutionClient || execution == protocol.ExecutionDelegated {
			function := &chatFunction{Name: definition.Name, Description: definition.Description, Parameters: definition.InputSchema}
			body.Tools = append(body.Tools, chatTool{Type: "function", Function: function})
			continue
		}
		if !kind.IsWeb() {
			return "", false, &protocol.Error{Code: protocol.ErrUnsupportedTool, Message: fmt.Sprintf("unknown tool kind %q", kind)}
		}
		if hostedKind != "" && hostedKind != kind {
			return "", false, &protocol.Error{Code: protocol.ErrUnsupportedTool, Message: "this Chat protocol supports one hosted web tool kind per request"}
		}
		hostedKind = kind
		switch provider {
		case "qwen", "dashscope", "alibaba":
			if kind != protocol.ToolWebSearch {
				return "", false, &protocol.Error{Code: protocol.ErrUnsupportedTool, Message: "Qwen Chat exposes hosted web_search only"}
			}
			body.EnableSearch = true
			body.SearchOptions = map[string]any{}
			if err := decodeOptions(definition.ProviderOptions, body.SearchOptions, map[string]bool{"search_strategy": true, "enable_source": true, "forced_search": true}); err != nil {
				return "", false, err
			}
		case "groq":
			if body.CompoundCustom == nil {
				body.CompoundCustom = &compoundCustom{}
			}
			name := "web_search"
			if kind == protocol.ToolWebFetch {
				name = "visit_website"
			}
			body.CompoundCustom.Tools.EnabledTools = append(body.CompoundCustom.Tools.EnabledTools, name)
			if len(definition.ProviderOptions) > 0 {
				return "", false, &protocol.Error{Code: protocol.ErrConfig, Message: "Groq compound web tools do not accept provider_options"}
			}
		case "openrouter":
			body.Tools = append(body.Tools, chatTool{Type: "openrouter:" + string(kind)})
		default:
			if kind != protocol.ToolWebSearch {
				return "", false, &protocol.Error{Code: protocol.ErrUnsupportedTool, Message: "OpenAI Chat exposes hosted web_search only"}
			}
			body.WebSearchOptions = map[string]any{"search_context_size": definition.ContextSize.Effective()}
			if len(definition.AllowedDomains) > 0 {
				body.WebSearchOptions["allowed_domains"] = definition.AllowedDomains
			}
			if definition.UserLocation != nil {
				body.WebSearchOptions["user_location"] = definition.UserLocation
			}
			if err := decodeOptions(definition.ProviderOptions, body.WebSearchOptions, map[string]bool{"include_sources": true}); err != nil {
				return "", false, err
			}
		}
	}
	return hostedKind, hostedKind.IsWeb(), nil
}

func decodeOptions(source map[string]json.RawMessage, target map[string]any, allowed map[string]bool) error {
	for name, raw := range source {
		if !allowed[name] {
			return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("unsupported web provider option %q", name)}
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("invalid web provider option %q", name), Cause: err}
		}
		target[name] = value
	}
	return nil
}

func emitChatParts(response protocol.ModelResponse, emit driver.EmitFunc, includeText bool) error {
	for _, part := range response.Turn.Parts {
		switch {
		case part.Kind == protocol.PartWebActivity && part.WebActivity != nil:
			activity := *part.WebActivity
			started := activity
			started.Status = protocol.WebStatusRunning
			startKind, completeKind := protocol.EventWebSearchStarted, protocol.EventWebSearchCompleted
			if activity.Kind == protocol.ToolWebFetch {
				startKind, completeKind = protocol.EventWebFetchStarted, protocol.EventWebFetchCompleted
			}
			if err := emit(protocol.ModelEvent{Kind: startKind, WebActivity: &started}); err != nil {
				return err
			}
			if err := emit(protocol.ModelEvent{Kind: completeKind, WebActivity: &activity}); err != nil {
				return err
			}
		case part.Kind == protocol.PartText && includeText:
			if err := emit(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: part.Text}); err != nil {
				return err
			}
		case part.Kind == protocol.PartCitation && part.Citation != nil:
			citation := *part.Citation
			if err := emit(protocol.ModelEvent{Kind: protocol.EventCitation, Citation: &citation}); err != nil {
				return err
			}
		}
	}
	return nil
}

func webUsage(kind protocol.ToolKind) protocol.WebUsage {
	if kind == protocol.ToolWebFetch {
		return protocol.WebUsage{Fetches: 1}
	}
	return protocol.WebUsage{Searches: 1}
}

func boundedRaw(raw []byte) json.RawMessage {
	const limit = 256 << 10
	if len(raw) > limit {
		raw = raw[:limit]
	}
	return append(json.RawMessage(nil), raw...)
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
	return protocol.ClassifyProviderHTTPError(status, message)
}

func mapHTTPErrorWithTools(status int, body []byte, hosted bool) error {
	if hosted && (status == http.StatusBadRequest || status == http.StatusUnprocessableEntity) {
		message := strings.ToLower(string(body))
		if (strings.Contains(message, "web_search") || strings.Contains(message, "web_fetch") || strings.Contains(message, "enable_search") || strings.Contains(message, "tool type")) && (strings.Contains(message, "unknown") || strings.Contains(message, "unsupported") || strings.Contains(message, "invalid")) {
			return &protocol.Error{Code: protocol.ErrUnsupportedTool, Message: "provider rejected the hosted web tool", StatusCode: status}
		}
	}
	return mapHTTPError(status, body)
}
