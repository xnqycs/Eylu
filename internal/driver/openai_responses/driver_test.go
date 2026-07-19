package openai_responses

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

func TestGenerateTextRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatal("missing bearer authorization")
		}
		var body requestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		first, ok := body.Input[0].(map[string]any)
		if body.Model != "test-model" || len(body.Input) != 1 || !ok || first["content"] != "hello" {
			t.Fatalf("request body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"world"}]}],"usage":{"input_tokens":2,"output_tokens":1}}`))
	}))
	defer server.Close()
	events := make([]protocol.EventKind, 0)
	response, err := New(server.Client()).Generate(context.Background(), driver.Request{
		BaseURL: server.URL + "/v1", APIKey: "test-key",
		Model: protocol.ModelRequest{ProtocolVersion: 1, Model: "test-model", Turns: []protocol.Turn{{
			Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "hello"}},
		}}},
	}, func(event protocol.ModelEvent) error {
		events = append(events, event.Kind)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Turn.Parts[0].Text != "world" || response.Usage.InputTokens != 2 {
		t.Fatalf("response = %#v", response)
	}
	if len(events) != 4 || events[0] != protocol.EventResponseStart || events[3] != protocol.EventResponseDone {
		t.Fatalf("events = %#v", events)
	}
}

func TestGenerateMapsHTTPAndTimeoutErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer server.Close()
	_, err := New(server.Client()).Generate(context.Background(), driver.Request{
		BaseURL: server.URL, APIKey: "bad", Model: protocol.ModelRequest{Model: "m", Turns: []protocol.Turn{{Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "x"}}}}},
	}, nil)
	if typed, ok := err.(*protocol.Error); !ok || typed.Code != protocol.ErrAuth || typed.StatusCode != http.StatusUnauthorized {
		t.Fatalf("error = %#v", err)
	}

	slowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer slowServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err = New(slowServer.Client()).Generate(ctx, driver.Request{
		BaseURL: slowServer.URL, APIKey: "x", Model: protocol.ModelRequest{Model: "m", Turns: []protocol.Turn{{Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "x"}}}}},
	}, nil)
	if typed, ok := err.(*protocol.Error); !ok || typed.Code != protocol.ErrTimeout {
		t.Fatalf("timeout error = %#v", err)
	}
}

func TestGenerateMapsToolHistory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		input := body["input"].([]any)
		if len(input) != 4 {
			t.Fatalf("input = %#v", input)
		}
		call := input[2].(map[string]any)
		output := input[3].(map[string]any)
		if call["type"] != "function_call" || call["arguments"] != `{"path":"main.go"}` || output["type"] != "function_call_output" || output["call_id"] != "call-1" || output["output"] != "package main" {
			t.Fatalf("call = %#v, output = %#v", call, output)
		}
		tools := body["tools"].([]any)
		if tools[0].(map[string]any)["name"] != "read_file" {
			t.Fatalf("tools = %#v", tools)
		}
		_, _ = w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}`))
	}))
	defer server.Close()
	call := protocol.ToolCall{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"main.go"}`)}
	result := protocol.ToolResult{CallID: "call-1", Content: "package main"}
	_, err := New(server.Client()).Generate(context.Background(), driver.Request{BaseURL: server.URL, APIKey: "key", Model: protocol.ModelRequest{
		Model: "model", Tools: []protocol.ToolDefinition{{Name: "read_file", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}}, Turns: []protocol.Turn{
			{Role: protocol.RoleSystem, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "system"}}},
			{Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "read"}}},
			{Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartToolCall, ToolCall: &call}}},
			{Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: &result}}},
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRemoteStateSendsOnlyNewInputAndChangedSystemBlocks(t *testing.T) {
	previous := []protocol.Turn{
		{ID: "system", Role: protocol.RoleSystem, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "base"}}},
		{ID: "project-map", Role: protocol.RoleSystem, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "old map"}}},
		{ID: "user-1", Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "old question"}}},
		{ID: "agent-1", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "old answer"}}},
	}
	state := remoteState{ResponseID: "resp_1", SystemDigests: systemTurnDigests(previous)}
	current := append([]protocol.Turn(nil), previous...)
	current[1] = protocol.Turn{ID: "project-map", Role: protocol.RoleSystem, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "new map"}}}
	current = append(current, protocol.Turn{ID: "user-2", Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "new question"}}})
	input := remoteInputTurns(current, state)
	if len(input) != 2 || input[0].ID != "project-map" || input[1].ID != "user-2" {
		t.Fatalf("remote input = %#v", input)
	}
	encoded := encodeRemoteState(json.RawMessage(`{"response_id":"resp_2"}`), current, false)
	decoded := decodeRemoteState(encoded)
	if decoded.ResponseID != "resp_2" || len(decoded.SystemDigests) != 2 {
		t.Fatalf("state = %#v", decoded)
	}
}

func TestGenerateFallsBackWhenHTTPGatewayRejectsPreviousResponse(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body requestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if requests == 1 {
			if body.PreviousResponseID != "resp_old" || len(body.Input) != 1 {
				t.Fatalf("incremental body = %#v", body)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"previous_response_id is only supported on Responses WebSocket v2"}}`))
			return
		}
		if body.PreviousResponseID != "" || len(body.Input) != 4 {
			t.Fatalf("fallback body = %#v", body)
		}
		_, _ = w.Write([]byte(`{"id":"resp_new","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer server.Close()
	turns := []protocol.Turn{
		{ID: "system", Role: protocol.RoleSystem, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "base"}}},
		{ID: "user-old", Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "old"}}},
		{ID: "agent-old", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "answer"}}},
		{ID: "user-new", Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "new"}}},
	}
	state, _ := json.Marshal(remoteState{ResponseID: "resp_old", SystemDigests: systemTurnDigests(turns)})
	response, err := New(server.Client()).Generate(context.Background(), driver.Request{BaseURL: server.URL, APIKey: "key", Model: protocol.ModelRequest{Model: "model", Turns: turns, DriverState: state}}, nil)
	if err != nil || requests != 2 || !decodeRemoteState(response.DriverState).DisablePrevious {
		t.Fatalf("response=%#v requests=%d err=%v", response, requests, err)
	}
}

func TestContextErrorBypassesResponsesFallbackAndStart(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"context_length_exceeded: maximum context length is 64000"}}`))
	}))
	defer server.Close()
	events := 0
	_, err := New(server.Client()).Generate(context.Background(), driver.Request{BaseURL: server.URL, Model: protocol.ModelRequest{Model: "model", Turns: []protocol.Turn{{Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "large"}}}}}}, func(protocol.ModelEvent) error {
		events++
		return nil
	})
	typed, ok := err.(*protocol.Error)
	if !ok || typed.Code != protocol.ErrContextWindow || typed.ContextLimit != 64000 || requests != 1 || events != 0 {
		t.Fatalf("error=%#v requests=%d events=%d", err, requests, events)
	}
}
