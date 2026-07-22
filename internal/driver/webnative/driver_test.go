package webnative

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

func TestNativeWebDriverContracts(t *testing.T) {
	tests := []struct {
		name      string
		dialect   Dialect
		path      string
		header    string
		toolTypes []string
		response  string
	}{
		{
			name: "anthropic", dialect: DialectAnthropic, path: "/v1/messages", header: "x-api-key", toolTypes: []string{"web_search_20260318", "web_fetch_20260318"},
			response: `{"id":"msg_1","content":[{"type":"server_tool_use","id":"web_1","name":"web_search","input":{"query":"Eylu"}},{"type":"web_search_tool_result","tool_use_id":"web_1","content":[{"type":"web_search_result","url":"https://example.com","title":"Example"}]},{"type":"text","text":"Eylu","citations":[{"type":"web_search_result_location","url":"https://example.com","title":"Example","start_index":0,"end_index":4}]}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1,"server_tool_use":{"web_search_requests":1}}}`,
		},
		{
			name: "gemini", dialect: DialectGemini, path: "/v1beta/interactions", header: "x-goog-api-key", toolTypes: []string{"google_search", "url_context"},
			response: genericNativeResponse("google_search_call"),
		},
		{
			name: "mistral", dialect: DialectMistral, path: "/v1/conversations", header: "authorization", toolTypes: []string{"web_search", "web_fetch"},
			response: genericNativeResponse("web_search_call"),
		},
		{
			name: "perplexity", dialect: DialectPerplexity, path: "/v1/agent", header: "authorization", toolTypes: []string{"web_search", "fetch_url"},
			response: genericNativeResponse("web_search_call"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path || r.Header.Get(tc.header) == "" {
					t.Fatalf("request path=%s headers=%v", r.URL.Path, r.Header)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				tools := body["tools"].([]any)
				for index, want := range tc.toolTypes {
					if tools[index].(map[string]any)["type"] != want {
						t.Fatalf("tools = %#v", tools)
					}
				}
				_, _ = w.Write([]byte(tc.response))
			}))
			defer server.Close()
			model := New(server.Client(), tc.dialect)
			request := driver.Request{BaseURL: server.URL + "/v1", APIKey: "secret", Stream: false, Model: protocol.ModelRequest{
				Model: "model", Turns: []protocol.Turn{{Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "search"}}}},
				Tools: []protocol.ToolDefinition{{Kind: protocol.ToolWebSearch, Execution: protocol.ExecutionHosted, MaxUses: 5}, {Kind: protocol.ToolWebFetch, Execution: protocol.ExecutionHosted, MaxUses: 5}},
			}}
			var events []protocol.EventKind
			response, err := model.Generate(context.Background(), request, func(event protocol.ModelEvent) error {
				events = append(events, event.Kind)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			activities, citations := 0, 0
			for _, part := range response.Turn.Parts {
				if part.WebActivity != nil {
					activities++
				}
				if part.Citation != nil {
					citations++
				}
			}
			if activities != 1 || citations != 1 || len(events) < 6 {
				t.Fatalf("response=%#v events=%#v", response, events)
			}
		})
	}
}

func TestNativeWebDriverCapabilities(t *testing.T) {
	for _, dialect := range []Dialect{DialectAnthropic, DialectGemini, DialectMistral, DialectPerplexity} {
		capabilities := New(nil, dialect).Capabilities()
		if !capabilities.HostedWebSearch || !capabilities.HostedWebFetch || !capabilities.HostedAndFunctionTools {
			t.Fatalf("dialect %s capabilities = %#v", dialect, capabilities)
		}
	}
}

func TestNativeWebDriverStreamLifecycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"web_search_call\",\"id\":\"web_1\",\"action\":{\"type\":\"search\",\"query\":\"Eylu\"}}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":" + genericNativeResponse("web_search_call") + "}\n\n"))
	}))
	defer server.Close()
	request := driver.Request{BaseURL: server.URL + "/v1", APIKey: "secret", Stream: true, Model: protocol.ModelRequest{
		Model: "model", Turns: []protocol.Turn{{Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "search"}}}},
		Tools: []protocol.ToolDefinition{{Kind: protocol.ToolWebSearch, Execution: protocol.ExecutionHosted}},
	}}
	var events []protocol.EventKind
	response, err := New(server.Client(), DialectMistral).Generate(context.Background(), request, func(event protocol.ModelEvent) error {
		events = append(events, event.Kind)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	starts, completions := 0, 0
	for _, kind := range events {
		if kind == protocol.EventWebSearchStarted {
			starts++
		}
		if kind == protocol.EventWebSearchCompleted {
			completions++
		}
	}
	if starts != 1 || completions != 1 || response.Turn.Parts[0].WebActivity == nil {
		t.Fatalf("response=%#v events=%#v", response, events)
	}
}

func TestAnthropicPauseTurnContinuesInsideGenerate(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		messages, _ := body["messages"].([]any)
		writer.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			if len(messages) != 1 {
				t.Fatalf("first messages=%#v", messages)
			}
			_, _ = writer.Write([]byte(`{"id":"pause","content":[{"type":"server_tool_use","id":"web_1","name":"web_search","input":{"query":"Eylu"}},{"type":"web_search_tool_result","tool_use_id":"web_1","content":[{"type":"web_search_result","url":"https://example.com","title":"Example"}]}],"stop_reason":"pause_turn","usage":{"input_tokens":5,"output_tokens":1}}`))
			return
		}
		if requests != 2 || len(messages) != 2 || messages[1].(map[string]any)["role"] != "assistant" {
			t.Fatalf("requests=%d messages=%#v", requests, messages)
		}
		_, _ = writer.Write([]byte(`{"id":"done","content":[{"type":"text","text":"continued answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer server.Close()
	request := driver.Request{BaseURL: server.URL + "/v1", APIKey: "secret", Model: protocol.ModelRequest{
		Model: "model", Turns: []protocol.Turn{{Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "search"}}}},
		Tools: []protocol.ToolDefinition{{Kind: protocol.ToolWebSearch, Execution: protocol.ExecutionHosted, MaxUses: 1}},
	}}
	var events []protocol.EventKind
	response, err := New(server.Client(), DialectAnthropic).Generate(context.Background(), request, func(event protocol.ModelEvent) error {
		events = append(events, event.Kind)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	starts, dones := 0, 0
	for _, event := range events {
		if event == protocol.EventResponseStart {
			starts++
		}
		if event == protocol.EventResponseDone {
			dones++
		}
	}
	if requests != 2 || starts != 1 || dones != 1 || response.Usage.InputTokens != 8 || len(response.Turn.Parts) != 2 || response.Turn.Parts[0].WebActivity == nil || response.Turn.Parts[1].Text != "continued answer" {
		t.Fatalf("requests=%d response=%#v events=%#v", requests, response, events)
	}
}

func genericNativeResponse(callType string) string {
	return `{"id":"response_1","output":[{"type":"` + callType + `","id":"web_1","status":"completed","action":{"type":"search","query":"Eylu"},"sources":[{"url":"https://example.com","title":"Example"}]},{"type":"message","content":[{"type":"output_text","text":"Eylu","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":4}]}]}],"usage":{"input_tokens":5,"output_tokens":1}}`
}
