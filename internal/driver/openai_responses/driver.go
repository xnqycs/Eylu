package openai_responses

import (
	"bytes"
	"context"
	"crypto/sha256"
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

const Name = "openai_responses"

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
	return driver.Capabilities{TextStreaming: true, ToolCalling: true, ParallelTools: true, Reasoning: true, ImageInput: true, RemoteSession: true}
}

type requestBody struct {
	Model              string `json:"model"`
	Input              []any  `json:"input"`
	Tools              []tool `json:"tools,omitempty"`
	Stream             bool   `json:"stream,omitempty"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
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
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type responseBody struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Output []responseItem `json:"output"`
	Usage  responseUsage  `json:"usage"`
}

type responseItem struct {
	Type      string `json:"type"`
	Role      string `json:"role"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type responseUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	OutputDetail struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (d *Driver) Generate(ctx context.Context, req driver.Request, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	body := requestBody{Model: req.Model.Model, Stream: req.Stream}
	state := decodeRemoteState(req.Model.DriverState)
	if !state.DisablePrevious {
		body.PreviousResponseID = state.ResponseID
	}
	appendResponseInput(&body, remoteInputTurns(req.Model.Turns, state))
	for _, def := range req.Model.Tools {
		body.Tools = append(body.Tools, tool{Type: "function", Name: def.Name, Description: def.Description, Parameters: def.InputSchema})
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "encode model request", Cause: err}
	}
	endpoint := strings.TrimRight(req.BaseURL, "/") + "/responses"
	if emit != nil {
		_ = emit(protocol.ModelEvent{Kind: protocol.EventResponseStart})
	}
	resp, err := d.send(ctx, endpoint, req, payload)
	if err != nil {
		return protocol.ModelResponse{}, mapTransportError(ctx, err)
	}
	disablePrevious := state.DisablePrevious
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if body.PreviousResponseID != "" && resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(string(raw)), "previous_response_id") {
			disablePrevious = true
			body.PreviousResponseID = ""
			body.Input = nil
			appendResponseInput(&body, req.Model.Turns)
			payload, err = json.Marshal(body)
			if err != nil {
				return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "encode fallback model request", Cause: err}
			}
			resp, err = d.send(ctx, endpoint, req, payload)
			if err != nil {
				return protocol.ModelResponse{}, mapTransportError(ctx, err)
			}
		} else {
			return protocol.ModelResponse{}, mapHTTPError(resp.StatusCode, raw)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return protocol.ModelResponse{}, mapHTTPError(resp.StatusCode, raw)
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
	result.DriverState = encodeRemoteState(result.DriverState, req.Model.Turns, disablePrevious)
	return result, nil
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
				body.Input = append(body.Input, functionCallOutput{Type: "function_call_output", CallID: part.ToolResult.CallID, Output: part.ToolResult.Content})
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
	return d.client.Do(httpReq)
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
		Turn:  protocol.Turn{ID: uuid.NewString(), Role: protocol.RoleAgent, CreatedAt: time.Now().UTC()},
		Stop:  protocol.StopCompleted,
		Usage: protocol.Usage{InputTokens: decoded.Usage.InputTokens, OutputTokens: decoded.Usage.OutputTokens, ReasoningTokens: decoded.Usage.OutputDetail.ReasoningTokens, Exact: true},
	}
	for _, item := range decoded.Output {
		switch item.Type {
		case "message":
			for _, content := range item.Content {
				if content.Type == "output_text" && content.Text != "" {
					result.Turn.Parts = append(result.Turn.Parts, protocol.Part{Kind: protocol.PartText, Text: content.Text})
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
		}
	}
	if decoded.ID != "" {
		result.DriverState, _ = json.Marshal(map[string]string{"response_id": decoded.ID})
	}
	return result
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
	code, retry := protocol.ErrProvider, status >= 500
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		code = protocol.ErrAuth
	}
	if status == http.StatusTooManyRequests {
		code, retry = protocol.ErrRateLimit, true
	}
	return &protocol.Error{Code: code, Message: fmt.Sprintf("provider HTTP %d: %s", status, message), Retryable: retry, StatusCode: status}
}
