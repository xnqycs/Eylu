package openai_responses

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

// This dialect carries the cache breakdown under input_tokens_details, and some
// gateways emit it under the Chat Completions name instead. Either way the figure
// is reported separately, because the input total already includes it.
func TestUsageReportsCachedInputTokensUnderEitherName(t *testing.T) {
	tests := []struct {
		name       string
		usage      string
		wantCached int
		wantInput  int
	}{
		{
			name:       "input_tokens_details",
			usage:      `"usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":64}}`,
			wantCached: 64,
			wantInput:  100,
		},
		{
			name:       "prompt_tokens_details",
			usage:      `"usage":{"input_tokens":100,"output_tokens":10,"prompt_tokens_details":{"cached_tokens":32}}`,
			wantCached: 32,
			wantInput:  100,
		},
		{
			name:       "no breakdown",
			usage:      `"usage":{"input_tokens":100,"output_tokens":10}`,
			wantCached: 0,
			wantInput:  100,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := `{"id":"r1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],` + test.usage + `}`
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(body))
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
			if response.Usage.CachedInputTokens != test.wantCached {
				t.Fatalf("cached = %d, want %d", response.Usage.CachedInputTokens, test.wantCached)
			}
			if response.Usage.InputTokens != test.wantInput {
				t.Fatalf("input = %d, want %d", response.Usage.InputTokens, test.wantInput)
			}
			if !response.Usage.Exact {
				t.Fatalf("a missing cache figure made the usage inexact: %#v", response.Usage)
			}
		})
	}
}
