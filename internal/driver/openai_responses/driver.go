package openai_responses

import (
	"bytes"
	"context"
	"crypto/sha256"
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

const Name = "openai_responses"

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
	return d.CapabilitiesFor(driver.CapabilityTarget{Provider: "openai"})
}

func (d *Driver) CapabilitiesFor(target driver.CapabilityTarget) driver.Capabilities {
	provider := strings.ToLower(strings.TrimSpace(target.Provider))
	capabilities := driver.Capabilities{
		TextStreaming: true, ToolCalling: true, ParallelTools: true, Reasoning: true, ImageInput: true, RemoteSession: true,
	}
	if provider == "openai" || provider == "xai" || provider == "openrouter" {
		capabilities.HostedWebSearch, capabilities.HostedWebFetch = true, true
		capabilities.HostedToolStreaming, capabilities.HostedAndFunctionTools = true, true
		capabilities.SearchDomainFilter, capabilities.SearchLocation, capabilities.SearchUsageDetails = true, true, true
	}
	return capabilities
}

type requestBody struct {
	Model              string           `json:"model"`
	Input              []any            `json:"input"`
	Tools              []tool           `json:"tools,omitempty"`
	ToolChoice         any              `json:"tool_choice,omitempty"`
	ParallelToolCalls  bool             `json:"parallel_tool_calls,omitempty"`
	Stream             bool             `json:"stream,omitempty"`
	PreviousResponseID string           `json:"previous_response_id,omitempty"`
	Reasoning          *reasoningConfig `json:"reasoning,omitempty"`
}

type reasoningConfig struct {
	Summary string `json:"summary,omitempty"`
	Effort  string `json:"effort,omitempty"`
}

type remoteState struct {
	ResponseID      string            `json:"response_id"`
	SystemDigests   map[string]string `json:"system_digests,omitempty"`
	DisablePrevious bool              `json:"disable_previous_response,omitempty"`
}

type inputItem struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type functionCallInput struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type functionCallOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type tool struct {
	Type                     string          `json:"type"`
	Name                     string          `json:"name,omitempty"`
	Description              string          `json:"description,omitempty"`
	Parameters               json.RawMessage `json:"parameters,omitempty"`
	Filters                  *webFilters     `json:"filters,omitempty"`
	UserLocation             *webLocation    `json:"user_location,omitempty"`
	SearchContextSize        string          `json:"search_context_size,omitempty"`
	MaxUses                  int             `json:"max_uses,omitempty"`
	ExternalWebAccess        *bool           `json:"external_web_access,omitempty"`
	SearchType               string          `json:"search_type,omitempty"`
	EnableImageUnderstanding *bool           `json:"enable_image_understanding,omitempty"`
	MaxResults               int             `json:"max_results,omitempty"`
	SearchPrompt             string          `json:"search_prompt,omitempty"`
}

type webFilters struct {
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	BlockedDomains []string `json:"blocked_domains,omitempty"`
}

type webLocation struct {
	Type     string `json:"type,omitempty"`
	Country  string `json:"country,omitempty"`
	Region   string `json:"region,omitempty"`
	City     string `json:"city,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

type responseBody struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Output []responseItem `json:"output"`
	Usage  *responseUsage `json:"usage"`
}

type responseItem struct {
	Type      string `json:"type"`
	Role      string `json:"role"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
	Action    struct {
		Type    string `json:"type"`
		Query   string `json:"query"`
		URL     string `json:"url"`
		Pattern string `json:"pattern"`
	} `json:"action"`
	Sources      []protocol.WebSource `json:"sources"`
	Content      []responseContent    `json:"content"`
	Raw          json.RawMessage      `json:"-"`
	RawTruncated bool                 `json:"-"`
}

type responseContent struct {
	Type        string               `json:"type"`
	Text        string               `json:"text"`
	Annotations []responseAnnotation `json:"annotations"`
}

type responseAnnotation struct {
	Type       string `json:"type"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
}

func (item *responseItem) UnmarshalJSON(data []byte) error {
	type alias responseItem
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*item = responseItem(decoded)
	item.RawTruncated = len(data) > 256<<10
	item.Raw = boundedRaw(data)
	return nil
}

type responseUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	OutputDetail struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	WebSearchCalls int `json:"web_search_calls"`
	WebFetchCalls  int `json:"web_fetch_calls"`
}

func (d *Driver) Generate(ctx context.Context, req driver.Request, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	body := requestBody{Model: req.Model.Model, Stream: req.Stream}
	parallelKey := requestCapabilityKey(req)
	if req.ParallelToolCalls && len(req.Model.Tools) > 0 {
		_, unsupported := d.parallelUnsupported.Load(parallelKey)
		body.ParallelToolCalls = !unsupported
	}
	effort := strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	if effort == "auto" {
		effort = ""
	}
	if req.Stream || effort != "" {
		body.Reasoning = &reasoningConfig{Effort: effort}
		if req.Stream {
			body.Reasoning.Summary = "auto"
		}
	}
	state := decodeRemoteState(req.Model.DriverState)
	if !state.DisablePrevious {
		body.PreviousResponseID = state.ResponseID
	}
	appendResponseInput(&body, remoteInputTurns(req.Model.Turns, state))
	hasHostedTools := false
	for _, def := range req.Model.Tools {
		mapped, hosted, mapErr := mapToolDefinition(def, req.Target)
		if mapErr != nil {
			return protocol.ModelResponse{}, mapErr
		}
		if def.ToolChoice.Effective() == protocol.ToolChoiceNone {
			continue
		}
		body.Tools = append(body.Tools, mapped)
		hasHostedTools = hasHostedTools || hosted
		if def.ToolChoice.Effective() == protocol.ToolChoiceRequired {
			if body.ToolChoice != nil {
				return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrConfig, Message: "only one required tool_choice is supported"}
			}
			body.ToolChoice = map[string]any{"type": mapped.Type}
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "encode model request", Cause: err}
	}
	endpoint := strings.TrimRight(req.BaseURL, "/") + "/responses"
	disablePrevious := state.DisablePrevious
	resp, err := d.send(ctx, endpoint, req, payload)
	if err != nil {
		return protocol.ModelResponse{}, mapTransportError(ctx, err)
	}
	for resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		mapped := mapHTTPError(resp.StatusCode, raw)
		var providerError *protocol.Error
		if errors.As(mapped, &providerError) && providerError.Code == protocol.ErrContextWindow {
			return protocol.ModelResponse{}, mapped
		}
		if body.ParallelToolCalls && parallelToolCallsUnsupported(resp.StatusCode, raw) {
			d.parallelUnsupported.Store(parallelKey, struct{}{})
			body.ParallelToolCalls = false
		} else if body.PreviousResponseID != "" && resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(string(raw)), "previous_response_id") {
			disablePrevious = true
			body.PreviousResponseID = ""
			body.Input = nil
			appendResponseInput(&body, req.Model.Turns)
		} else if body.Reasoning != nil && body.Reasoning.Summary != "" && resp.StatusCode == http.StatusBadRequest && reasoningSummaryUnsupported(raw, body.Reasoning.Effort != "") {
			body.Reasoning.Summary = ""
			if body.Reasoning.Effort == "" {
				body.Reasoning = nil
			}
		} else {
			return protocol.ModelResponse{}, mapHTTPErrorWithTools(resp.StatusCode, raw, hasHostedTools)
		}
		payload, err = json.Marshal(body)
		if err != nil {
			return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "encode fallback model request", Cause: err}
		}
		resp, err = d.send(ctx, endpoint, req, payload)
		if err != nil {
			return protocol.ModelResponse{}, mapTransportError(ctx, err)
		}
	}
	defer resp.Body.Close()
	if emit != nil {
		if err := emit(protocol.ModelEvent{Kind: protocol.EventResponseStart}); err != nil {
			return protocol.ModelResponse{}, err
		}
	}
	if req.Stream {
		result, streamErr := d.readStream(ctx, resp.Body, emit)
		if streamErr != nil {
			return protocol.ModelResponse{}, streamErr
		}
		result.DriverState = encodeRemoteState(result.DriverState, req.Model.Turns, disablePrevious)
		if emit != nil {
			if emitErr := emit(protocol.ModelEvent{Kind: protocol.EventResponseDone, Response: &result}); emitErr != nil {
				return protocol.ModelResponse{}, emitErr
			}
		}
		return result, nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrNetwork, Message: "read model response", Retryable: true, Cause: err}
	}
	var decoded responseBody
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "decode model response", Cause: err}
	}
	if decoded.Error != nil {
		return protocol.ModelResponse{}, protocol.ClassifyProviderMessage(decoded.Error.Message)
	}
	result := convertResponse(decoded)
	if len(result.Turn.Parts) == 0 {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "provider returned no text or tool calls"}
	}
	if emit != nil {
		if err := emitResponseParts(result, emit, nil, true); err != nil {
			return protocol.ModelResponse{}, err
		}
		if err := emit(protocol.ModelEvent{Kind: protocol.EventUsage, Usage: &result.Usage}); err != nil {
			return protocol.ModelResponse{}, err
		}
		if err := emit(protocol.ModelEvent{Kind: protocol.EventResponseDone, Response: &result}); err != nil {
			return protocol.ModelResponse{}, err
		}
	}
	result.DriverState = encodeRemoteState(result.DriverState, req.Model.Turns, disablePrevious)
	return result, nil
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

func reasoningSummaryUnsupported(raw []byte, explicitEffort bool) bool {
	message := strings.ToLower(string(raw))
	unsupported := strings.Contains(message, "unsupported") || strings.Contains(message, "unknown") || strings.Contains(message, "unrecognized") || strings.Contains(message, "invalid")
	if !unsupported {
		return false
	}
	if strings.Contains(message, "summary") {
		return true
	}
	return !explicitEffort && strings.Contains(message, "reasoning")
}

func appendResponseInput(body *requestBody, turns []protocol.Turn) {
	for _, turn := range turns {
		for _, part := range turn.Parts {
			switch {
			case part.Kind == protocol.PartText && part.Text != "":
				role := string(turn.Role)
				if turn.Role == protocol.RoleAgent {
					role = "assistant"
				}
				body.Input = append(body.Input, inputItem{Role: role, Content: part.Text})
			case part.Kind == protocol.PartToolCall && part.ToolCall != nil:
				body.Input = append(body.Input, functionCallInput{Type: "function_call", CallID: part.ToolCall.ID, Name: part.ToolCall.Name, Arguments: string(part.ToolCall.Arguments)})
			case part.Kind == protocol.PartToolResult && part.ToolResult != nil:
				body.Input = append(body.Input, functionCallOutput{Type: "function_call_output", CallID: part.ToolResult.CallID, Output: driver.ToolResultContent(*part.ToolResult)})
			}
		}
	}
}

func (d *Driver) send(ctx context.Context, endpoint string, req driver.Request, payload []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrConfig, Message: "build model request", Cause: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	client := d.client
	if req.Stream {
		client = driver.StreamingHTTPClient(client)
	}
	return client.Do(httpReq)
}

func decodeRemoteState(raw json.RawMessage) remoteState {
	var state remoteState
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &state)
	}
	if state.SystemDigests == nil {
		state.SystemDigests = make(map[string]string)
	}
	return state
}

func encodeRemoteState(responseState json.RawMessage, turns []protocol.Turn, disablePrevious bool) json.RawMessage {
	state := decodeRemoteState(responseState)
	state.SystemDigests = systemTurnDigests(turns)
	state.DisablePrevious = disablePrevious
	encoded, _ := json.Marshal(state)
	return encoded
}

func remoteInputTurns(turns []protocol.Turn, state remoteState) []protocol.Turn {
	if state.ResponseID == "" || state.DisablePrevious {
		return turns
	}
	result := make([]protocol.Turn, 0)
	currentSystem := systemTurnDigests(turns)
	for _, turn := range turns {
		if turn.Role == protocol.RoleSystem && state.SystemDigests[turn.ID] != currentSystem[turn.ID] {
			result = append(result, turn)
		}
	}
	lastAgent := -1
	for index := len(turns) - 1; index >= 0; index-- {
		if turns[index].Role == protocol.RoleAgent {
			lastAgent = index
			break
		}
	}
	for index := lastAgent + 1; index < len(turns); index++ {
		if turns[index].Role != protocol.RoleSystem {
			result = append(result, turns[index])
		}
	}
	return result
}

func systemTurnDigests(turns []protocol.Turn) map[string]string {
	digests := make(map[string]string)
	for _, turn := range turns {
		if turn.Role != protocol.RoleSystem {
			continue
		}
		hash := sha256.New()
		for _, part := range turn.Parts {
			hash.Write([]byte(part.Kind))
			hash.Write([]byte{0})
			hash.Write([]byte(part.Text))
		}
		digests[turn.ID] = fmt.Sprintf("%x", hash.Sum(nil))
	}
	return digests
}

func convertResponse(decoded responseBody) protocol.ModelResponse {
	result := protocol.ModelResponse{
		Turn: protocol.Turn{ID: uuid.NewString(), Role: protocol.RoleAgent, CreatedAt: time.Now().UTC()},
		Stop: protocol.StopCompleted,
	}
	if decoded.Usage != nil {
		result.Usage = protocol.Usage{
			InputTokens: decoded.Usage.InputTokens, OutputTokens: decoded.Usage.OutputTokens,
			ReasoningTokens: decoded.Usage.OutputDetail.ReasoningTokens, Exact: true,
		}
	}
	lastWebCall := ""
	for _, item := range decoded.Output {
		switch item.Type {
		case "message":
			for _, content := range item.Content {
				if content.Type == "output_text" && content.Text != "" {
					result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartText, Text: content.Text})
				}
				for _, annotation := range content.Annotations {
					if annotation.Type != "url_citation" || annotation.URL == "" {
						continue
					}
					citation := protocol.URLCitation{CallID: lastWebCall, URL: annotation.URL, Title: annotation.Title, StartIndex: annotation.StartIndex, EndIndex: annotation.EndIndex}
					result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartCitation, Citation: &citation})
				}
			}
		case "function_call":
			callID := item.CallID
			if callID == "" {
				callID = item.ID
			}
			call := protocol.ToolCall{ID: callID, Name: item.Name, Arguments: json.RawMessage(item.Arguments)}
			result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartToolCall, ToolCall: &call})
			result.Stop = protocol.StopToolUse
		case "web_search_call", "web_fetch_call", "openrouter_web_search_call", "openrouter_web_fetch_call":
			activity := webActivityFromItem(item, decoded.Usage)
			lastWebCall = activity.CallID
			result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartWebActivity, WebActivity: &activity})
		}
	}
	if decoded.ID != "" {
		result.DriverState, _ = json.Marshal(map[string]string{"response_id": decoded.ID})
	}
	return result
}

func mapToolDefinition(definition protocol.ToolDefinition, target driver.CapabilityTarget) (tool, bool, error) {
	kind := definition.Kind.Effective()
	if kind == protocol.ToolFunction || definition.Execution.Effective() == protocol.ExecutionClient || definition.Execution.Effective() == protocol.ExecutionDelegated {
		return tool{Type: "function", Name: definition.Name, Description: definition.Description, Parameters: definition.InputSchema}, false, nil
	}
	if !kind.IsWeb() {
		return tool{}, false, &protocol.Error{Code: protocol.ErrUnsupportedTool, Message: fmt.Sprintf("unknown tool kind %q", kind)}
	}
	provider := strings.ToLower(strings.TrimSpace(target.Provider))
	typeName := string(kind)
	if provider == "openrouter" {
		typeName = "openrouter:" + string(kind)
	}
	mapped := tool{Type: typeName, SearchContextSize: string(definition.ContextSize.Effective()), MaxUses: definition.MaxUses}
	if len(definition.AllowedDomains) > 0 || len(definition.BlockedDomains) > 0 {
		mapped.Filters = &webFilters{AllowedDomains: definition.AllowedDomains, BlockedDomains: definition.BlockedDomains}
	}
	if location := definition.UserLocation; location != nil {
		mapped.UserLocation = &webLocation{Type: "approximate", Country: location.Country, Region: location.Region, City: location.City, Timezone: location.Timezone}
	}
	for name, raw := range definition.ProviderOptions {
		switch name {
		case "external_web_access":
			if err := json.Unmarshal(raw, &mapped.ExternalWebAccess); err != nil {
				return tool{}, false, invalidProviderOption(name, err)
			}
		case "search_type":
			if provider != "xai" {
				return tool{}, false, invalidProviderOption(name, nil)
			}
			if err := json.Unmarshal(raw, &mapped.SearchType); err != nil {
				return tool{}, false, invalidProviderOption(name, err)
			}
		case "enable_image_understanding":
			if provider != "xai" {
				return tool{}, false, invalidProviderOption(name, nil)
			}
			if err := json.Unmarshal(raw, &mapped.EnableImageUnderstanding); err != nil {
				return tool{}, false, invalidProviderOption(name, err)
			}
		case "max_results":
			if provider != "openrouter" || json.Unmarshal(raw, &mapped.MaxResults) != nil {
				return tool{}, false, invalidProviderOption(name, nil)
			}
		case "search_prompt":
			if provider != "openrouter" || json.Unmarshal(raw, &mapped.SearchPrompt) != nil {
				return tool{}, false, invalidProviderOption(name, nil)
			}
		default:
			return tool{}, false, invalidProviderOption(name, nil)
		}
	}
	return mapped, true, nil
}

func invalidProviderOption(name string, cause error) error {
	return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("unsupported or invalid web provider option %q", name), Cause: cause}
}

func webActivityFromItem(item responseItem, usage *responseUsage) protocol.WebActivity {
	kind := protocol.ToolWebSearch
	if strings.Contains(item.Type, "fetch") {
		kind = protocol.ToolWebFetch
	}
	status := protocol.WebStatus(item.Status)
	if status == "" || status == "succeeded" {
		status = protocol.WebStatusCompleted
	}
	activity := protocol.WebActivity{
		CallID: item.ID, Kind: kind, Query: item.Action.Query, URL: item.Action.URL, Action: item.Action.Type,
		Status: status, Sources: append([]protocol.WebSource(nil), item.Sources...), RawProviderResponse: append(json.RawMessage(nil), item.Raw...), RawTruncated: item.RawTruncated,
	}
	if activity.Action == "" {
		activity.Action = "search"
		if kind == protocol.ToolWebFetch {
			activity.Action = "open_page"
		}
	}
	if usage != nil {
		activity.Usage = protocol.WebUsage{Searches: usage.WebSearchCalls, Fetches: usage.WebFetchCalls}
		if activity.Usage.Searches == 0 && kind == protocol.ToolWebSearch {
			activity.Usage.Searches = 1
		}
		if activity.Usage.Fetches == 0 && kind == protocol.ToolWebFetch {
			activity.Usage.Fetches = 1
		}
	}
	return activity
}

func emitResponseParts(response protocol.ModelResponse, emit driver.EmitFunc, started map[string]bool, includeText bool) error {
	for _, part := range response.Turn.Parts {
		switch {
		case part.Kind == protocol.PartText && includeText:
			if err := emit(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: part.Text}); err != nil {
				return err
			}
		case part.Kind == protocol.PartWebActivity && part.WebActivity != nil:
			activity := *part.WebActivity
			if started == nil || !started[activity.CallID] {
				start := activity
				start.Status = protocol.WebStatusRunning
				startKind := protocol.EventWebSearchStarted
				if activity.Kind == protocol.ToolWebFetch {
					startKind = protocol.EventWebFetchStarted
				}
				if err := emit(protocol.ModelEvent{Kind: startKind, WebActivity: &start}); err != nil {
					return err
				}
			}
			completedKind := protocol.EventWebSearchCompleted
			if activity.Kind == protocol.ToolWebFetch {
				completedKind = protocol.EventWebFetchCompleted
			}
			if err := emit(protocol.ModelEvent{Kind: completedKind, WebActivity: &activity}); err != nil {
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

func boundedRaw(raw []byte) json.RawMessage {
	const limit = 256 << 10
	if len(raw) > limit {
		raw = raw[:limit]
	}
	return append(json.RawMessage(nil), raw...)
}

func mapTransportError(ctx context.Context, err error) error {
	if ctx.Err() == context.Canceled {
		return &protocol.Error{Code: protocol.ErrCancelled, Message: "model request cancelled", Cause: ctx.Err()}
	}
	if ctx.Err() == context.DeadlineExceeded {
		return &protocol.Error{Code: protocol.ErrTimeout, Message: "model request timed out", Retryable: true, Cause: ctx.Err()}
	}
	return &protocol.Error{Code: protocol.ErrNetwork, Message: "model request failed", Retryable: true, Cause: err}
}

func mapHTTPError(status int, body []byte) error {
	message := strings.TrimSpace(string(body))
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
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
		mentionsWeb := strings.Contains(message, "web_search") || strings.Contains(message, "web_fetch") || strings.Contains(message, "tool type")
		rejected := strings.Contains(message, "unknown") || strings.Contains(message, "unsupported") || strings.Contains(message, "invalid") || strings.Contains(message, "unrecognized")
		if mentionsWeb && rejected {
			return &protocol.Error{Code: protocol.ErrUnsupportedTool, Message: "provider rejected the hosted web tool", StatusCode: status}
		}
	}
	return mapHTTPError(status, body)
}
