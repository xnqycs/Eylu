package webnative

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

type Dialect string

const (
	DialectAnthropic  Dialect = "anthropic"
	DialectGemini     Dialect = "gemini"
	DialectMistral    Dialect = "mistral"
	DialectPerplexity Dialect = "perplexity"
)

type Driver struct {
	client  *http.Client
	dialect Dialect
	name    string
}

func New(client *http.Client, dialect Dialect) *Driver {
	if client == nil {
		client = http.DefaultClient
	}
	return &Driver{client: client, dialect: dialect, name: string(dialect)}
}

func NewNamed(client *http.Client, dialect Dialect, name string) *Driver {
	model := New(client, dialect)
	model.name = name
	return model
}

func (d *Driver) Name() string { return d.name }

func (d *Driver) Capabilities() driver.Capabilities {
	capabilities := driver.Capabilities{
		TextStreaming: true, ToolCalling: true, ParallelTools: true, Reasoning: true,
		HostedWebSearch: true, HostedWebFetch: true, HostedToolStreaming: true, HostedAndFunctionTools: true,
		SearchUsageDetails: true,
	}
	switch d.dialect {
	case DialectAnthropic, DialectPerplexity:
		capabilities.SearchDomainFilter, capabilities.SearchLocation = true, true
	}
	return capabilities
}

func (d *Driver) CapabilitiesFor(driver.CapabilityTarget) driver.Capabilities {
	return d.Capabilities()
}

func (d *Driver) Generate(ctx context.Context, request driver.Request, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	body, err := d.requestBody(request)
	if err != nil {
		return protocol.ModelResponse{}, err
	}
	maximumContinuations := hostedUseLimit(request.Model.Tools)
	var aggregate protocol.ModelResponse
	responseStarted := false
	for continuation := 0; ; continuation++ {
		payload, err := json.Marshal(body)
		if err != nil {
			return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "encode native web request", Cause: err}
		}
		httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint(request.BaseURL), bytes.NewReader(payload))
		if err != nil {
			return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrConfig, Message: "build native web request", Cause: err}
		}
		d.applyHeaders(httpRequest, request)
		client := d.client
		if request.Stream {
			client = driver.StreamingHTTPClient(client)
		}
		response, err := client.Do(httpRequest)
		if err != nil {
			return protocol.ModelResponse{}, mapTransportError(ctx, err)
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			return protocol.ModelResponse{}, mapHTTPError(response.StatusCode, raw, hasHosted(request.Model.Tools))
		}
		if emit != nil && !responseStarted {
			if err := emit(protocol.ModelEvent{Kind: protocol.EventResponseStart}); err != nil {
				response.Body.Close()
				return protocol.ModelResponse{}, err
			}
			responseStarted = true
		}
		var result protocol.ModelResponse
		var raw json.RawMessage
		if request.Stream {
			result, raw, err = d.readStream(ctx, response.Body, emit)
		} else {
			raw, err = io.ReadAll(io.LimitReader(response.Body, 8<<20))
			if err == nil {
				result, err = d.convert(raw)
			}
		}
		response.Body.Close()
		if err != nil {
			return protocol.ModelResponse{}, err
		}
		if emit != nil && !request.Stream {
			if err := emitParts(result, emit, nil, true); err != nil {
				return protocol.ModelResponse{}, err
			}
		}
		aggregateNativeResponse(&aggregate, result)
		if d.dialect == DialectAnthropic && anthropicPause(raw) {
			if continuation >= maximumContinuations {
				return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrTool, Message: "Anthropic hosted web max_uses exceeded during pause_turn continuation"}
			}
			if err := appendAnthropicContinuation(body, raw); err != nil {
				return protocol.ModelResponse{}, err
			}
			continue
		}
		if len(aggregate.Turn.Parts) == 0 {
			return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "provider returned no text, web activity, or tool calls"}
		}
		if emit != nil {
			if err := emit(protocol.ModelEvent{Kind: protocol.EventUsage, Usage: &aggregate.Usage}); err != nil {
				return protocol.ModelResponse{}, err
			}
			if err := emit(protocol.ModelEvent{Kind: protocol.EventResponseDone, Response: &aggregate}); err != nil {
				return protocol.ModelResponse{}, err
			}
		}
		return aggregate, nil
	}
}

func hostedUseLimit(definitions []protocol.ToolDefinition) int {
	maximum := 0
	for _, definition := range definitions {
		if definition.Kind.Effective().IsWeb() && definition.MaxUses > 0 && (maximum == 0 || definition.MaxUses < maximum) {
			maximum = definition.MaxUses
		}
	}
	if maximum == 0 {
		maximum = 5
	}
	return maximum
}

func aggregateNativeResponse(target *protocol.ModelResponse, next protocol.ModelResponse) {
	if target.Turn.ID == "" {
		target.Turn = next.Turn
	} else {
		target.Turn.Parts = append(target.Turn.Parts, next.Turn.Parts...)
	}
	target.Stop = next.Stop
	target.Usage.InputTokens += next.Usage.InputTokens
	target.Usage.OutputTokens += next.Usage.OutputTokens
	target.Usage.ReasoningTokens += next.Usage.ReasoningTokens
	target.Usage.Exact = target.Usage.Exact || next.Usage.Exact
	target.DriverState = append(target.DriverState[:0], next.DriverState...)
}

func anthropicPause(raw json.RawMessage) bool {
	var envelope struct {
		StopReason string `json:"stop_reason"`
	}
	return json.Unmarshal(raw, &envelope) == nil && envelope.StopReason == "pause_turn"
}

func appendAnthropicContinuation(body map[string]any, raw json.RawMessage) error {
	var envelope struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.Content) == 0 {
		return &protocol.Error{Code: protocol.ErrProtocol, Message: "decode Anthropic pause_turn content", Cause: err}
	}
	messages, _ := body["messages"].([]any)
	messages = append(messages, map[string]any{"role": "assistant", "content": envelope.Content})
	body["messages"] = messages
	return nil
}

func (d *Driver) requestBody(request driver.Request) (map[string]any, error) {
	body := map[string]any{"model": request.Model.Model, "stream": request.Stream}
	messages := make([]any, 0, len(request.Model.Turns))
	for _, turn := range request.Model.Turns {
		if d.dialect == DialectAnthropic && turn.Role == protocol.RoleSystem {
			body["system"] = turnText(turn)
			continue
		}
		message := map[string]any{"role": roleName(turn.Role)}
		blocks := make([]any, 0, len(turn.Parts))
		for _, part := range turn.Parts {
			switch {
			case part.Kind == protocol.PartText && part.Text != "":
				blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
			case part.Kind == protocol.PartToolCall && part.ToolCall != nil:
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": part.ToolCall.ID, "name": part.ToolCall.Name, "input": json.RawMessage(part.ToolCall.Arguments)})
			case part.Kind == protocol.PartToolResult && part.ToolResult != nil:
				blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": part.ToolResult.CallID, "content": driver.ToolResultContent(*part.ToolResult), "is_error": part.ToolResult.IsError})
			}
		}
		if len(blocks) == 1 {
			if block, ok := blocks[0].(map[string]any); ok && block["type"] == "text" {
				message["content"] = block["text"]
			} else {
				message["content"] = blocks
			}
		} else if len(blocks) > 0 {
			message["content"] = blocks
		}
		if _, ok := message["content"]; ok {
			messages = append(messages, message)
		}
	}
	if d.dialect == DialectGemini {
		body["input"] = messages
	} else if d.dialect == DialectMistral {
		body["inputs"] = messages
	} else {
		body["messages"] = messages
	}
	if d.dialect == DialectAnthropic {
		body["max_tokens"] = 4096
	}
	tools := make([]any, 0, len(request.Model.Tools))
	for _, definition := range request.Model.Tools {
		mapped, err := d.mapTool(definition)
		if err != nil {
			return nil, err
		}
		if mapped != nil {
			tools = append(tools, mapped)
		}
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	return body, nil
}

func (d *Driver) mapTool(definition protocol.ToolDefinition) (map[string]any, error) {
	if definition.ToolChoice.Effective() == protocol.ToolChoiceNone {
		return nil, nil
	}
	kind := definition.Kind.Effective()
	if kind == protocol.ToolFunction || definition.Execution.Effective() == protocol.ExecutionClient || definition.Execution.Effective() == protocol.ExecutionDelegated {
		if d.dialect == DialectAnthropic {
			return map[string]any{"name": definition.Name, "description": definition.Description, "input_schema": definition.InputSchema}, nil
		}
		return map[string]any{"type": "function", "name": definition.Name, "description": definition.Description, "parameters": definition.InputSchema}, nil
	}
	if !kind.IsWeb() {
		return nil, &protocol.Error{Code: protocol.ErrUnsupportedTool, Message: fmt.Sprintf("unknown tool kind %q", kind)}
	}
	typeName := d.toolType(kind)
	mapped := map[string]any{"type": typeName}
	if d.dialect == DialectAnthropic {
		mapped["name"] = string(kind)
	}
	if definition.MaxUses > 0 {
		mapped["max_uses"] = definition.MaxUses
	}
	if len(definition.AllowedDomains) > 0 {
		mapped["allowed_domains"] = definition.AllowedDomains
	}
	if len(definition.BlockedDomains) > 0 {
		mapped["blocked_domains"] = definition.BlockedDomains
	}
	if definition.UserLocation != nil {
		mapped["user_location"] = definition.UserLocation
	}
	for name, raw := range definition.ProviderOptions {
		allowed := false
		switch d.dialect {
		case DialectAnthropic:
			allowed = name == "version"
		case DialectMistral:
			allowed = name == "premium"
		case DialectGemini:
			allowed = name == "dynamic_retrieval_config"
		case DialectPerplexity:
			allowed = name == "search_recency_filter" || name == "search_mode"
		}
		if !allowed {
			return nil, &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("unsupported %s web provider option %q", d.dialect, name)}
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("invalid web provider option %q", name), Cause: err}
		}
		if name == "version" {
			version, ok := value.(string)
			if !ok || !regexp.MustCompile(`^[0-9]{8}$`).MatchString(version) {
				return nil, &protocol.Error{Code: protocol.ErrConfig, Message: "Anthropic web tool version must use YYYYMMDD"}
			}
			mapped["type"] = string(kind) + "_" + version
		} else if name == "premium" {
			if premium, _ := value.(bool); premium && kind == protocol.ToolWebSearch {
				mapped["type"] = "web_search_premium"
			}
		} else {
			mapped[name] = value
		}
	}
	return mapped, nil
}

func (d *Driver) toolType(kind protocol.ToolKind) string {
	switch d.dialect {
	case DialectAnthropic:
		if kind == protocol.ToolWebFetch {
			return "web_fetch_20260318"
		}
		return "web_search_20260318"
	case DialectGemini:
		if kind == protocol.ToolWebFetch {
			return "url_context"
		}
		return "google_search"
	case DialectPerplexity:
		if kind == protocol.ToolWebFetch {
			return "fetch_url"
		}
		return "web_search"
	default:
		return string(kind)
	}
}

func (d *Driver) endpoint(baseURL string) string {
	base := strings.TrimRight(baseURL, "/")
	switch d.dialect {
	case DialectAnthropic:
		return base + "/messages"
	case DialectGemini:
		base = strings.TrimSuffix(base, "/v1")
		return base + "/v1beta/interactions"
	case DialectMistral:
		return base + "/conversations"
	case DialectPerplexity:
		return base + "/agent"
	default:
		return base
	}
}

func (d *Driver) applyHeaders(request *http.Request, source driver.Request) {
	request.Header.Set("Content-Type", "application/json")
	switch d.dialect {
	case DialectAnthropic:
		request.Header.Set("x-api-key", source.APIKey)
		request.Header.Set("anthropic-version", "2023-06-01")
	case DialectGemini:
		request.Header.Set("x-goog-api-key", source.APIKey)
	default:
		request.Header.Set("Authorization", "Bearer "+source.APIKey)
	}
	for name, value := range source.Headers {
		request.Header.Set(name, value)
	}
}

func (d *Driver) convert(raw []byte) (protocol.ModelResponse, error) {
	if d.dialect == DialectAnthropic {
		return convertAnthropic(raw)
	}
	return convertOutputItems(raw)
}

type nativeEnvelope struct {
	ID     string       `json:"id"`
	Output []nativeItem `json:"output"`
	Usage  nativeUsage  `json:"usage"`
	Error  *nativeError `json:"error"`
}

type nativeError struct {
	Message string `json:"message"`
}

type nativeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type nativeItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
	Action    struct {
		Type  string `json:"type"`
		Query string `json:"query"`
		URL   string `json:"url"`
	} `json:"action"`
	Sources      []protocol.WebSource `json:"sources"`
	Content      []nativeContent      `json:"content"`
	Raw          json.RawMessage      `json:"-"`
	RawTruncated bool                 `json:"-"`
}

type nativeContent struct {
	Type        string             `json:"type"`
	Text        string             `json:"text"`
	Annotations []nativeAnnotation `json:"annotations"`
}

type nativeAnnotation struct {
	Type       string `json:"type"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
}

func (item *nativeItem) UnmarshalJSON(data []byte) error {
	type alias nativeItem
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*item = nativeItem(decoded)
	item.RawTruncated = len(data) > 256<<10
	item.Raw = boundedRaw(data)
	return nil
}

func convertOutputItems(raw []byte) (protocol.ModelResponse, error) {
	var decoded nativeEnvelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "decode native web response", Cause: err}
	}
	if decoded.Error != nil {
		return protocol.ModelResponse{}, protocol.ClassifyProviderMessage(decoded.Error.Message)
	}
	result := protocol.ModelResponse{Turn: protocol.Turn{ID: uuid.NewString(), Role: protocol.RoleAgent, CreatedAt: time.Now().UTC()}, Stop: protocol.StopCompleted, Usage: protocol.Usage{InputTokens: decoded.Usage.InputTokens, OutputTokens: decoded.Usage.OutputTokens, Exact: decoded.Usage.InputTokens > 0 || decoded.Usage.OutputTokens > 0}}
	lastCall := ""
	for _, item := range decoded.Output {
		switch {
		case strings.Contains(item.Type, "search_call"), strings.Contains(item.Type, "fetch_call"), strings.Contains(item.Type, "url_context_call"):
			kind := protocol.ToolWebSearch
			if strings.Contains(item.Type, "fetch") || strings.Contains(item.Type, "url_context") {
				kind = protocol.ToolWebFetch
			}
			status := protocol.WebStatus(item.Status)
			if status == "" || status == "succeeded" {
				status = protocol.WebStatusCompleted
			}
			activity := protocol.WebActivity{CallID: item.ID, Kind: kind, Query: item.Action.Query, URL: item.Action.URL, Action: item.Action.Type, Status: status, Sources: item.Sources, Usage: webUsage(kind), RawProviderResponse: item.Raw, RawTruncated: item.RawTruncated}
			lastCall = activity.CallID
			result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartWebActivity, WebActivity: &activity})
		case item.Type == "message":
			for _, content := range item.Content {
				if content.Text != "" {
					result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartText, Text: content.Text})
				}
				for _, annotation := range content.Annotations {
					if annotation.Type == "url_citation" && annotation.URL != "" {
						citation := protocol.URLCitation{CallID: lastCall, URL: annotation.URL, Title: annotation.Title, StartIndex: annotation.StartIndex, EndIndex: annotation.EndIndex}
						result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartCitation, Citation: &citation})
					}
				}
			}
		case item.Type == "function_call", item.Type == "tool_call":
			callID := item.CallID
			if callID == "" {
				callID = item.ID
			}
			call := protocol.ToolCall{ID: callID, Name: item.Name, Arguments: json.RawMessage(item.Arguments)}
			result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartToolCall, ToolCall: &call})
			result.Stop = protocol.StopToolUse
		}
	}
	return result, nil
}

type anthropicEnvelope struct {
	ID         string             `json:"id"`
	Content    []anthropicContent `json:"content"`
	StopReason string             `json:"stop_reason"`
	Usage      struct {
		InputTokens   int `json:"input_tokens"`
		OutputTokens  int `json:"output_tokens"`
		ServerToolUse struct {
			WebSearchRequests int `json:"web_search_requests"`
			WebFetchRequests  int `json:"web_fetch_requests"`
		} `json:"server_tool_use"`
	} `json:"usage"`
	Error *nativeError `json:"error"`
}

type anthropicContent struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Text      string          `json:"text"`
	Content   []struct {
		Type  string `json:"type"`
		URL   string `json:"url"`
		Title string `json:"title"`
		Text  string `json:"encrypted_content"`
	} `json:"content"`
	Citations []nativeAnnotation `json:"citations"`
}

func convertAnthropic(raw []byte) (protocol.ModelResponse, error) {
	var decoded anthropicEnvelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "decode Anthropic response", Cause: err}
	}
	if decoded.Error != nil {
		return protocol.ModelResponse{}, protocol.ClassifyProviderMessage(decoded.Error.Message)
	}
	result := protocol.ModelResponse{Turn: protocol.Turn{ID: uuid.NewString(), Role: protocol.RoleAgent, CreatedAt: time.Now().UTC()}, Stop: protocol.StopCompleted, Usage: protocol.Usage{InputTokens: decoded.Usage.InputTokens, OutputTokens: decoded.Usage.OutputTokens, Exact: true}}
	activities := make(map[string]*protocol.WebActivity)
	order := make([]string, 0)
	for _, content := range decoded.Content {
		switch content.Type {
		case "server_tool_use":
			kind := protocol.ToolWebSearch
			if strings.Contains(content.Name, "fetch") {
				kind = protocol.ToolWebFetch
			}
			var input struct {
				Query string `json:"query"`
				URL   string `json:"url"`
			}
			_ = json.Unmarshal(content.Input, &input)
			activity := &protocol.WebActivity{CallID: content.ID, Kind: kind, Query: input.Query, URL: input.URL, Action: actionFor(kind), Status: protocol.WebStatusRunning, Usage: webUsage(kind)}
			activities[content.ID], order = activity, append(order, content.ID)
		case "web_search_tool_result", "web_fetch_tool_result":
			activity := activities[content.ToolUseID]
			if activity == nil {
				kind := protocol.ToolWebSearch
				if strings.Contains(content.Type, "fetch") {
					kind = protocol.ToolWebFetch
				}
				activity = &protocol.WebActivity{CallID: content.ToolUseID, Kind: kind, Action: actionFor(kind), Usage: webUsage(kind)}
				activities[content.ToolUseID], order = activity, append(order, content.ToolUseID)
			}
			activity.Status = protocol.WebStatusCompleted
			for _, source := range content.Content {
				if source.URL != "" {
					activity.Sources = append(activity.Sources, protocol.WebSource{URL: source.URL, Title: source.Title})
				}
			}
		case "text":
			if content.Text != "" {
				result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartText, Text: content.Text})
			}
			callID := ""
			if len(order) > 0 {
				callID = order[len(order)-1]
			}
			for _, item := range content.Citations {
				if item.URL != "" {
					citation := protocol.URLCitation{CallID: callID, URL: item.URL, Title: item.Title, StartIndex: item.StartIndex, EndIndex: item.EndIndex}
					result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartCitation, Citation: &citation})
				}
			}
		case "tool_use":
			call := protocol.ToolCall{ID: content.ID, Name: content.Name, Arguments: content.Input}
			result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartToolCall, ToolCall: &call})
			result.Stop = protocol.StopToolUse
		}
	}
	activityParts := make([]protocol.Part, 0, len(order))
	for _, id := range order {
		activity := activities[id]
		if activity.Status == protocol.WebStatusRunning {
			activity.Status = protocol.WebStatusCompleted
		}
		activity.RawProviderResponse = boundedRaw(raw)
		activity.RawTruncated = len(raw) > 256<<10
		activityParts = append(activityParts, protocol.Part{Kind: protocol.PartWebActivity, WebActivity: activity})
	}
	result.Turn.Parts = append(activityParts, result.Turn.Parts...)
	return result, nil
}

func (d *Driver) readStream(ctx context.Context, body io.Reader, emit driver.EmitFunc) (protocol.ModelResponse, json.RawMessage, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	started := make(map[string]bool)
	var final *protocol.ModelResponse
	var terminalRaw json.RawMessage
	var text strings.Builder
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Delta    json.RawMessage `json:"delta"`
			Item     json.RawMessage `json:"item"`
			Response json.RawMessage `json:"response"`
			Message  json.RawMessage `json:"message"`
			Error    *nativeError    `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return protocol.ModelResponse{}, nil, &protocol.Error{Code: protocol.ErrProtocol, Message: "decode native web stream event", Cause: err}
		}
		if event.Error != nil {
			return protocol.ModelResponse{}, nil, protocol.ClassifyProviderMessage(event.Error.Message)
		}
		var delta struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(event.Delta, &delta)
		if delta.Text != "" {
			text.WriteString(delta.Text)
			if emit != nil {
				if err := emit(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: delta.Text}); err != nil {
					return protocol.ModelResponse{}, nil, err
				}
			}
		}
		if len(event.Item) > 0 {
			var item nativeItem
			if json.Unmarshal(event.Item, &item) == nil && (strings.Contains(item.Type, "search_call") || strings.Contains(item.Type, "fetch_call") || strings.Contains(item.Type, "url_context_call")) {
				kind := protocol.ToolWebSearch
				if strings.Contains(item.Type, "fetch") || strings.Contains(item.Type, "url_context") {
					kind = protocol.ToolWebFetch
				}
				activity := protocol.WebActivity{CallID: item.ID, Kind: kind, Query: item.Action.Query, URL: item.Action.URL, Action: actionFor(kind), Status: protocol.WebStatusRunning, RawProviderResponse: item.Raw, RawTruncated: item.RawTruncated}
				started[item.ID] = true
				if emit != nil {
					kindEvent := protocol.EventWebSearchStarted
					if kind == protocol.ToolWebFetch {
						kindEvent = protocol.EventWebFetchStarted
					}
					if err := emit(protocol.ModelEvent{Kind: kindEvent, WebActivity: &activity}); err != nil {
						return protocol.ModelResponse{}, nil, err
					}
				}
			}
		}
		finalRaw := event.Response
		if len(finalRaw) == 0 {
			finalRaw = event.Message
		}
		if len(finalRaw) > 0 && (strings.Contains(event.Type, "completed") || strings.Contains(event.Type, "done") || strings.Contains(event.Type, "stop")) {
			converted, err := d.convert(finalRaw)
			if err != nil {
				return protocol.ModelResponse{}, nil, err
			}
			if emit != nil {
				if err := emitParts(converted, emit, started, false); err != nil {
					return protocol.ModelResponse{}, nil, err
				}
			}
			final = &converted
			terminalRaw = append(terminalRaw[:0], finalRaw...)
		}
	}
	if err := scanner.Err(); err != nil {
		return protocol.ModelResponse{}, nil, mapTransportError(ctx, err)
	}
	if final == nil {
		return protocol.ModelResponse{}, nil, &protocol.Error{Code: protocol.ErrNetwork, Message: "native web stream ended before a final response", Retryable: true}
	}
	return *final, terminalRaw, nil
}

func emitParts(response protocol.ModelResponse, emit driver.EmitFunc, started map[string]bool, includeText bool) error {
	for _, part := range response.Turn.Parts {
		switch {
		case part.WebActivity != nil:
			activity := *part.WebActivity
			startKind, completeKind := protocol.EventWebSearchStarted, protocol.EventWebSearchCompleted
			if activity.Kind == protocol.ToolWebFetch {
				startKind, completeKind = protocol.EventWebFetchStarted, protocol.EventWebFetchCompleted
			}
			if started == nil || !started[activity.CallID] {
				start := activity
				start.Status = protocol.WebStatusRunning
				if err := emit(protocol.ModelEvent{Kind: startKind, WebActivity: &start}); err != nil {
					return err
				}
			}
			if err := emit(protocol.ModelEvent{Kind: completeKind, WebActivity: &activity}); err != nil {
				return err
			}
		case part.Kind == protocol.PartText && includeText:
			if err := emit(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: part.Text}); err != nil {
				return err
			}
		case part.Citation != nil:
			citation := *part.Citation
			if err := emit(protocol.ModelEvent{Kind: protocol.EventCitation, Citation: &citation}); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasHosted(definitions []protocol.ToolDefinition) bool {
	for _, definition := range definitions {
		if definition.Kind.Effective().IsWeb() && definition.Execution.Effective() == protocol.ExecutionHosted {
			return true
		}
	}
	return false
}

func webUsage(kind protocol.ToolKind) protocol.WebUsage {
	if kind == protocol.ToolWebFetch {
		return protocol.WebUsage{Fetches: 1}
	}
	return protocol.WebUsage{Searches: 1}
}

func actionFor(kind protocol.ToolKind) string {
	if kind == protocol.ToolWebFetch {
		return "open_page"
	}
	return "search"
}

func roleName(role protocol.Role) string {
	if role == protocol.RoleAgent {
		return "assistant"
	}
	return string(role)
}

func turnText(turn protocol.Turn) string {
	var result strings.Builder
	for _, part := range turn.Parts {
		if part.Kind == protocol.PartText {
			result.WriteString(part.Text)
		}
	}
	return result.String()
}

func boundedRaw(raw []byte) json.RawMessage {
	const limit = 256 << 10
	if len(raw) > limit {
		raw = raw[:limit]
	}
	return append(json.RawMessage(nil), raw...)
}

func mapTransportError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return &protocol.Error{Code: protocol.ErrCancelled, Message: "native web request cancelled", Cause: ctx.Err()}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &protocol.Error{Code: protocol.ErrTimeout, Message: "native web request timed out", Retryable: true, Cause: ctx.Err()}
	}
	return &protocol.Error{Code: protocol.ErrNetwork, Message: "native web request failed", Retryable: true, Cause: err}
}

func mapHTTPError(status int, raw []byte, hosted bool) error {
	message := strings.TrimSpace(string(raw))
	var envelope struct {
		Error *nativeError `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error != nil && envelope.Error.Message != "" {
		message = envelope.Error.Message
	}
	lower := strings.ToLower(message)
	if hosted && (status == http.StatusBadRequest || status == http.StatusUnprocessableEntity) && (strings.Contains(lower, "web_search") || strings.Contains(lower, "web_fetch") || strings.Contains(lower, "google_search") || strings.Contains(lower, "url_context")) && (strings.Contains(lower, "unsupported") || strings.Contains(lower, "unknown") || strings.Contains(lower, "invalid")) {
		return &protocol.Error{Code: protocol.ErrUnsupportedTool, Message: "provider rejected the hosted web tool", StatusCode: status}
	}
	return protocol.ClassifyProviderHTTPError(status, message)
}

var _ driver.ModelDriver = (*Driver)(nil)
var _ driver.TargetCapabilityDriver = (*Driver)(nil)
