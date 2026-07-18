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
		if body.Model != "test-model" || len(body.Input) != 1 || body.Input[0].Content != "hello" {
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
