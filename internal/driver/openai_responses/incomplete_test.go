package openai_responses

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

// An incomplete envelope is a response the provider stopped early. It must never
// be reported as a normal completion, and the calls it carried must not look
// executable.
func TestIncompleteEnvelopeMapsToATruncatedStop(t *testing.T) {
	tests := []struct {
		name string
		body string
		want protocol.StopKind
	}{
		{
			name: "completed",
			body: `{"id":"r1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`,
			want: protocol.StopCompleted,
		},
		{
			name: "incomplete at the token limit",
			body: `{"id":"r1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`,
			want: protocol.StopLength,
		},
		{
			name: "incomplete by content filter",
			body: `{"id":"r1","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`,
			want: protocol.StopLength,
		},
		{
			name: "failed",
			body: `{"id":"r1","status":"failed","output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`,
			want: protocol.StopError,
		},
		{
			name: "cancelled",
			body: `{"id":"r1","status":"cancelled","output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`,
			want: protocol.StopCancelled,
		},
		{
			name: "unrecognized status",
			body: `{"id":"r1","status":"queued","output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`,
			want: protocol.StopLength,
		},
		{
			name: "incomplete with a function call",
			body: `{"id":"r1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"function_call","call_id":"call-1","name":"echo","arguments":"{\"value\":\"partial"}]}`,
			want: protocol.StopLength,
		},
		{
			name: "completed with a function call",
			body: `{"id":"r1","status":"completed","output":[{"type":"function_call","call_id":"call-1","name":"echo","arguments":"{}"}]}`,
			want: protocol.StopToolUse,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			request := driver.Request{
				BaseURL: server.URL, APIKey: "key",
				Model: protocol.ModelRequest{Model: "model", Turns: []protocol.Turn{{Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "hi"}}}}},
			}
			response, err := New(server.Client()).Generate(context.Background(), request, nil)
			if err != nil {
				t.Fatal(err)
			}
			if response.Stop != test.want {
				t.Fatalf("stop = %q, want %q", response.Stop, test.want)
			}
			if len(response.Turn.Parts) == 0 {
				t.Fatal("the partial response was dropped")
			}
		})
	}
}
