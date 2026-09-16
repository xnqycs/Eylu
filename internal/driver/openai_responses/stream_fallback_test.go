package openai_responses

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

// A gateway can end the stream with [DONE] and never send a terminal envelope.
// The response is then synthesized from the deltas, and its stopping condition
// comes from the same policy as every other one: the dialect expresses tool use
// as content, so a stream carrying calls asks for them and one carrying only text
// is a completion.
//
// This path is the one the contract table does not reach, because the table drives
// the terminal envelope. It is pinned here so the single mapping entry point stays
// the only place that decides a stop kind.
func TestStreamWithoutATerminalEnvelopeStillDecidesItsStopThroughThePolicy(t *testing.T) {
	tests := []struct {
		name     string
		events   []string
		want     protocol.StopKind
		wantCall bool
	}{
		{
			name:   "text only",
			events: []string{`{"type":"response.output_text.delta","delta":"hello"}`, `[DONE]`},
			want:   protocol.StopCompleted,
		},
		{
			name: "a tool call",
			events: []string{
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call-1","name":"echo","arguments":""}}`,
				`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`,
				`[DONE]`,
			},
			want:     protocol.StopToolUse,
			wantCall: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				for _, event := range test.events {
					_, _ = writer.Write([]byte("data: " + event + "\n\n"))
				}
			}))
			defer server.Close()

			request := driver.Request{
				BaseURL: server.URL, APIKey: "key", Stream: true,
				Model: protocol.ModelRequest{Model: "model", Turns: []protocol.Turn{{
					Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "hello"}},
				}}},
			}
			response, err := New(server.Client()).Generate(context.Background(), request, nil)
			if err != nil {
				t.Fatal(err)
			}
			if response.Stop != test.want {
				t.Fatalf("stop = %q, want %q", response.Stop, test.want)
			}
			// The conclusion must be the one the shared policy produces for the
			// reason this dialect derives from the same content.
			reason := driver.StopReasonCompleted
			calls := 0
			for _, part := range response.Turn.Parts {
				if part.Kind == protocol.PartToolCall && part.ToolCall != nil {
					calls++
				}
			}
			if test.wantCall {
				reason = driver.StopReasonToolUse
			}
			kind, interop, policyErr := driver.StopKindFor(reason, calls > 0, false)
			if policyErr != nil || kind != response.Stop {
				t.Fatalf("the driver disagreed with the policy: kind=%q interop=%q err=%v", kind, interop, policyErr)
			}
			if len(response.Interop) != 0 {
				t.Fatalf("a synthesized response claimed a relaxation it did not need: %#v", response.Interop)
			}
			if calls == 0 && test.wantCall {
				t.Fatal("the synthesized response lost the call the stream carried")
			}
		})
	}
}
