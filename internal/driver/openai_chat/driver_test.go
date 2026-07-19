package openai_chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

func TestChatRequestHistoryAndResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var body chatRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Messages) != 3 || body.Messages[0].Role != "system" || body.Messages[1].Role != "user" || body.Messages[2].Role != "assistant" {
			t.Fatalf("messages = %#v", body.Messages)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":1}}`))
	}))
	defer server.Close()
	response, err := New(server.Client()).Generate(context.Background(), driver.Request{
		BaseURL: server.URL + "/v1", APIKey: "key", Model: protocol.ModelRequest{Model: "model", Turns: []protocol.Turn{
			{Role: protocol.RoleSystem, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "system"}}},
			{Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "question"}}},
			{Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "prior"}}},
		}},
	}, nil)
	if err != nil || response.Turn.Parts[0].Text != "ok" || response.Usage.InputTokens != 7 {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
}

func TestChatStreamMergesTextAndRejectsDisconnect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\",\"content\":\"he\"},\"finish_reason\":\"\"}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"llo\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":1}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	var streamed, reasoning strings.Builder
	request := driver.Request{BaseURL: server.URL, APIKey: "key", Stream: true, Model: protocol.ModelRequest{Model: "model", Turns: []protocol.Turn{{Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "hi"}}}}}}
	response, err := New(server.Client()).Generate(context.Background(), request, func(event protocol.ModelEvent) error {
		if event.Kind == protocol.EventTextDelta {
			streamed.WriteString(event.Delta)
		}
		if event.Kind == protocol.EventReasoningDelta {
			reasoning.WriteString(event.Delta)
		}
		return nil
	})
	if err != nil || streamed.String() != "hello" || reasoning.String() != "thinking" || response.Turn.Parts[0].Text != "hello" || response.Usage.InputTokens != 4 {
		t.Fatalf("stream = %q, reasoning = %q, response = %#v, err = %v", streamed.String(), reasoning.String(), response, err)
	}

	disconnected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
	}))
	defer disconnected.Close()
	request.BaseURL = disconnected.URL
	_, err = New(disconnected.Client()).Generate(context.Background(), request, nil)
	if typed, ok := err.(*protocol.Error); !ok || typed.Code != protocol.ErrNetwork {
		t.Fatalf("disconnect error = %#v", err)
	}
}

func TestChatStreamEmitsToolArgumentDeltasAndOutlivesClientTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"write_file\",\"arguments\":\"{\\\"path\\\":\"}}]},\"finish_reason\":\"\"}]}\n\n"))
		w.(http.Flusher).Flush()
		time.Sleep(75 * time.Millisecond)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"index.html\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	client := server.Client()
	client.Timeout = 20 * time.Millisecond
	request := driver.Request{BaseURL: server.URL, APIKey: "key", Stream: true, Model: protocol.ModelRequest{Model: "model", Turns: []protocol.Turn{{Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "create"}}}}}}
	var deltas []protocol.ToolCallDelta
	response, err := New(client).Generate(context.Background(), request, func(event protocol.ModelEvent) error {
		if event.Kind == protocol.EventToolCallDelta && event.ToolCallDelta != nil {
			deltas = append(deltas, *event.ToolCallDelta)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Stop != protocol.StopToolUse || len(response.Turn.Parts) != 1 || len(deltas) != 3 {
		t.Fatalf("response=%#v deltas=%#v", response, deltas)
	}
	if deltas[0].Delta+deltas[1].Delta != `{"path":"index.html"}` || !deltas[2].Done || deltas[2].Arguments != `{"path":"index.html"}` {
		t.Fatalf("deltas = %#v", deltas)
	}
	if client.Timeout != 20*time.Millisecond {
		t.Fatalf("shared client timeout = %s", client.Timeout)
	}
}

func TestChatMapsToolHistory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Messages) != 4 || len(body.Messages[2].ToolCalls) != 1 || body.Messages[2].ToolCalls[0].Function.Arguments != `{"path":"main.go"}` || body.Messages[3].Role != "tool" || body.Messages[3].ToolCallID != "call-1" || body.Messages[3].Content != "package main" {
			t.Fatalf("messages = %#v", body.Messages)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	call := protocol.ToolCall{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"main.go"}`)}
	result := protocol.ToolResult{CallID: "call-1", Content: "package main"}
	_, err := New(server.Client()).Generate(context.Background(), driver.Request{BaseURL: server.URL, APIKey: "key", Model: protocol.ModelRequest{Model: "model", Turns: []protocol.Turn{
		{Role: protocol.RoleSystem, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "system"}}},
		{Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "read"}}},
		{Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartToolCall, ToolCall: &call}}},
		{Role: protocol.RoleTool, Parts: []protocol.Part{{Kind: protocol.PartToolResult, ToolResult: &result}}},
	}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
}
