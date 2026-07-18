package openai_chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"he\"},\"finish_reason\":\"\"}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"llo\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":1}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	var streamed strings.Builder
	request := driver.Request{BaseURL: server.URL, APIKey: "key", Stream: true, Model: protocol.ModelRequest{Model: "model", Turns: []protocol.Turn{{Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "hi"}}}}}}
	response, err := New(server.Client()).Generate(context.Background(), request, func(event protocol.ModelEvent) error {
		if event.Kind == protocol.EventTextDelta {
			streamed.WriteString(event.Delta)
		}
		return nil
	})
	if err != nil || streamed.String() != "hello" || response.Turn.Parts[0].Text != "hello" || response.Usage.InputTokens != 4 {
		t.Fatalf("stream = %q, response = %#v, err = %v", streamed.String(), response, err)
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
