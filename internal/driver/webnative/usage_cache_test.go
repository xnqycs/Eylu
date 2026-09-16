package webnative

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

// The Anthropic dialect names the cached part of the prompt
// cache_read_input_tokens, and it is already inside input_tokens.
func TestAnthropicUsageReportsCachedInputTokensSeparately(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"msg_1","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":120,"output_tokens":9,"cache_read_input_tokens":96}}`))
	}))
	defer server.Close()

	response, err := New(server.Client(), DialectAnthropic).Generate(context.Background(), driver.Request{
		BaseURL: server.URL + "/v1", APIKey: "secret",
		Model: protocol.ModelRequest{Model: "model", Turns: []protocol.Turn{{
			Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "hi"}},
		}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage.InputTokens != 120 || response.Usage.CachedInputTokens != 96 {
		t.Fatalf("usage = %#v", response.Usage)
	}
	if response.Usage.CachedInputTokens > response.Usage.InputTokens {
		t.Fatalf("the cache figure exceeded the input it is a part of: %#v", response.Usage)
	}
}

// The dialects without a cache breakdown leave the figure at 0, which is a
// missing number rather than an inexact one.
func TestDialectWithoutACacheBreakdownReportsZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(genericNativeResponse("web_search_call")))
	}))
	defer server.Close()

	response, err := New(server.Client(), DialectMistral).Generate(context.Background(), driver.Request{
		BaseURL: server.URL + "/v1", APIKey: "secret",
		Model: protocol.ModelRequest{Model: "model", Turns: []protocol.Turn{{
			Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "hi"}},
		}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage.CachedInputTokens != 0 {
		t.Fatalf("cached = %d, want none", response.Usage.CachedInputTokens)
	}
	if !response.Usage.Exact {
		t.Fatalf("a missing cache figure made the usage inexact: %#v", response.Usage)
	}
}
