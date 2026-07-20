package openai_responses

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

func TestResponsesStreamTextAndToolArguments(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		server := streamServer([]string{
			`{"type":"response.created"}`,
			`{"type":"response.reasoning_summary_text.delta","delta":"thinking "}`,
			`{"type":"response.reasoning_summary_text.delta","delta":"now"}`,
			`{"type":"response.output_text.delta","delta":""}`,
			`{"type":"response.output_text.delta","delta":"hel"}`,
			`{"type":"response.output_text.delta","delta":"lo"}`,
			`{"type":"response.completed","response":{"id":"resp_1","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":5,"output_tokens":1}}}`,
		})
		defer server.Close()
		var streamed, reasoning strings.Builder
		response, err := New(server.Client()).Generate(context.Background(), streamRequest(server.URL), func(event protocol.ModelEvent) error {
			if event.Kind == protocol.EventTextDelta {
				streamed.WriteString(event.Delta)
			}
			if event.Kind == protocol.EventReasoningDelta {
				reasoning.WriteString(event.Delta)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if streamed.String() != "hello" || reasoning.String() != "thinking now" || response.Turn.Parts[0].Text != "hello" || response.Usage.InputTokens != 5 {
			t.Fatalf("stream = %q, reasoning = %q, response = %#v", streamed.String(), reasoning.String(), response)
		}
	})

	t.Run("tool", func(t *testing.T) {
		server := streamServer([]string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":"}`,
			`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"main.go\"}"}`,
			`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"path\":\"main.go\"}"}`,
			`[DONE]`,
		})
		defer server.Close()
		var deltas []protocol.ToolCallDelta
		response, err := New(server.Client()).Generate(context.Background(), streamRequest(server.URL), func(event protocol.ModelEvent) error {
			if event.Kind == protocol.EventToolCallDelta && event.ToolCallDelta != nil {
				deltas = append(deltas, *event.ToolCallDelta)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		call := response.Turn.Parts[0].ToolCall
		if response.Stop != protocol.StopToolUse || call.ID != "call_1" || call.Name != "read_file" || string(call.Arguments) != `{"path":"main.go"}` {
			t.Fatalf("response = %#v", response)
		}
		if len(deltas) != 3 || deltas[0].Name != "read_file" || deltas[0].ID != "call_1" {
			t.Fatalf("tool deltas = %#v", deltas)
		}
		if deltas[1].Delta != `{"path":"main.go"}` || !deltas[2].Done || deltas[2].Arguments != `{"path":"main.go"}` {
			t.Fatalf("tool deltas = %#v", deltas)
		}
	})
}

func TestResponsesStreamRequestsReasoningSummaryAndFallsBack(t *testing.T) {
	t.Run("requests automatic summary", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Reasoning *struct {
					Summary string `json:"summary"`
					Effort  string `json:"effort"`
				} `json:"reasoning"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Reasoning == nil || body.Reasoning.Summary != "auto" || body.Reasoning.Effort != "" {
				t.Errorf("reasoning = %#v", body.Reasoning)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}]}}\n\n"))
		}))
		defer server.Close()

		response, err := New(server.Client()).Generate(context.Background(), streamRequest(server.URL), nil)
		if err != nil {
			t.Fatal(err)
		}
		if response.Usage.Exact {
			t.Fatalf("usage without provider data was marked exact: %#v", response.Usage)
		}
	})

	t.Run("retries without summary after bad request", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempt := requests.Add(1)
			var body struct {
				Reasoning *struct {
					Summary string `json:"summary"`
					Effort  string `json:"effort"`
				} `json:"reasoning"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if attempt == 1 {
				if body.Reasoning == nil || body.Reasoning.Summary != "auto" || body.Reasoning.Effort != "high" {
					t.Errorf("first reasoning = %#v", body.Reasoning)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"unsupported parameter reasoning.summary"}}`))
				return
			}
			if body.Reasoning == nil || body.Reasoning.Summary != "" || body.Reasoning.Effort != "high" {
				t.Errorf("fallback reasoning = %#v", body.Reasoning)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}]}}\n\n"))
		}))
		defer server.Close()

		request := streamRequest(server.URL)
		request.ReasoningEffort = "high"
		response, err := New(server.Client()).Generate(context.Background(), request, nil)
		if err != nil {
			t.Fatal(err)
		}
		if response.Usage.Exact {
			t.Fatalf("fallback usage without provider data was marked exact: %#v", response.Usage)
		}
		if requests.Load() != 2 {
			t.Fatalf("requests = %d", requests.Load())
		}
	})

	t.Run("effort rejection is returned without retry", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unsupported reasoning effort ultra"}}`))
		}))
		defer server.Close()
		request := streamRequest(server.URL)
		request.ReasoningEffort = "ultra"
		_, err := New(server.Client()).Generate(context.Background(), request, nil)
		if err == nil || requests.Load() != 1 {
			t.Fatalf("error=%v requests=%d", err, requests.Load())
		}
	})
}

func TestStreamDeltaBufferBatchesTinyFragments(t *testing.T) {
	var buffer driver.StreamDeltaBuffer
	started := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 23; index++ {
		if batch, ready := buffer.Push("x", started); ready || batch != "" {
			t.Fatalf("unexpected early batch at %d: %q", index, batch)
		}
	}
	batch, ready := buffer.Push("x", started.Add(250*time.Millisecond))
	if !ready || batch != strings.Repeat("x", 24) {
		t.Fatalf("delayed batch = %q ready=%t", batch, ready)
	}
	for index := 0; index < 256; index++ {
		batch, ready = buffer.Push("y", started)
	}
	if !ready || batch != strings.Repeat("y", 256) || buffer.Flush() != "" {
		t.Fatalf("maximum batch = %q ready=%t tail=%q", batch, ready, buffer.Flush())
	}
}

func TestResponsesStreamDisconnectAndEmpty(t *testing.T) {
	server := streamServer([]string{`{"type":"response.output_text.delta","delta":"partial"}`})
	defer server.Close()
	_, err := New(server.Client()).Generate(context.Background(), streamRequest(server.URL), nil)
	if typed, ok := err.(*protocol.Error); !ok || typed.Code != protocol.ErrNetwork || !typed.Retryable {
		t.Fatalf("disconnect error = %#v", err)
	}

	empty := streamServer([]string{`[DONE]`})
	defer empty.Close()
	_, err = New(empty.Client()).Generate(context.Background(), streamRequest(empty.URL), nil)
	if typed, ok := err.(*protocol.Error); !ok || typed.Code != protocol.ErrProtocol {
		t.Fatalf("empty error = %#v", err)
	}
}

func TestResponsesStreamOutlivesClientTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\"}\n\n"))
		w.(http.Flusher).Flush()
		time.Sleep(75 * time.Millisecond)
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}]}}\n\n"))
	}))
	defer server.Close()

	client := server.Client()
	client.Timeout = 20 * time.Millisecond
	response, err := New(client).Generate(context.Background(), streamRequest(server.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	if responseText(response) != "done" {
		t.Fatalf("response = %#v", response)
	}
	if client.Timeout != 20*time.Millisecond {
		t.Fatalf("shared client timeout = %s", client.Timeout)
	}
}

func streamServer(events []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			_, _ = w.Write([]byte("data: " + event + "\n\n"))
		}
	}))
}

func streamRequest(baseURL string) driver.Request {
	return driver.Request{BaseURL: baseURL, APIKey: "key", Stream: true, Model: protocol.ModelRequest{Model: "model", Turns: []protocol.Turn{{Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "hello"}}}}}}
}
