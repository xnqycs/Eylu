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
