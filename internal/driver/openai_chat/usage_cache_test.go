package openai_chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

// A cached prompt token is reported on its own. It is already inside
// prompt_tokens, so the input total must not grow because of it, and a provider
// that reports no breakdown leaves the figure at 0 rather than making the usage
// inexact.
func TestUsageReportsCachedInputTokensWithoutDoubleCounting(t *testing.T) {
	tests := []struct {
		name string
		body string
		want protocol.Usage
	}{
		{
			name: "cache hit reported",
			body: `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":80}}}`,
			want: protocol.Usage{InputTokens: 100, OutputTokens: 10, CachedInputTokens: 80, Exact: true},
		},
		{
			name: "no cache breakdown",
			body: `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`,
			want: protocol.Usage{InputTokens: 100, OutputTokens: 10, Exact: true},
		},
		{
			name: "an empty cache breakdown is not a cache hit",
			body: `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":0}}}`,
			want: protocol.Usage{InputTokens: 100, OutputTokens: 10, Exact: true},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()

			response, err := New(server.Client()).Generate(context.Background(), driver.Request{
				BaseURL: server.URL, APIKey: "key",
				Model: protocol.ModelRequest{Model: "model", Turns: []protocol.Turn{{
					Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "hi"}},
				}}},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if response.Usage != test.want {
				t.Fatalf("usage = %#v, want %#v", response.Usage, test.want)
			}
			if response.Usage.CachedInputTokens > response.Usage.InputTokens {
				t.Fatalf("the cache figure exceeded the input it is a part of: %#v", response.Usage)
			}
		})
	}
}
