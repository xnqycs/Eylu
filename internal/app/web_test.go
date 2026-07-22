package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"Eylu/internal/config"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/webtool"
)

func TestDelegatedWebBackendUsesTargetHostedCapability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" {
			t.Fatalf("path=%s", request.URL.Path)
		}
		var body struct {
			Tools []map[string]any `json:"tools"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Tools) != 1 || body.Tools[0]["type"] != "web_search" {
			t.Fatalf("tools=%#v", body.Tools)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"delegated","status":"completed","output":[{"type":"web_search_call","id":"search-1","status":"completed","action":{"type":"search","query":"Eylu"},"sources":[{"url":"https://example.com","title":"Example"}]},{"type":"message","content":[{"type":"output_text","text":"delegated answer","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":9}]}]}],"usage":{"input_tokens":4,"output_tokens":2,"web_search_calls":1}}`))
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.ActiveProvider = "backup"
	cfg.Providers["backup"] = config.ProviderConfig{
		Adapter: "openai_responses", BaseURL: server.URL + "/v1", Model: "web-model", CatalogProvider: "openai",
		WebTools: config.WebToolsConfig{Permission: config.WebPermissionAllow},
	}
	manager, err := provider.NewManager(filepath.Join(t.TempDir(), "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	resolved := webtool.ResolvedTool{
		Definition: protocol.ToolDefinition{Kind: protocol.ToolWebSearch, Name: "web_search", Execution: protocol.ExecutionDelegated, MaxUses: 3},
		Execution:  protocol.ExecutionDelegated, Target: "backup",
	}
	result := (&runtime{}).delegatedWebBackend(manager)(context.Background(), resolved, json.RawMessage(`{"query":"Eylu"}`))
	if result.IsError || result.Content != "delegated answer" || result.Metadata["citation_count"] != 1 || result.Metadata["web_input_tokens"] != 4 {
		t.Fatalf("result=%#v", result)
	}
	var structured struct {
		Activities []protocol.WebActivity `json:"activities"`
		Citations  []protocol.URLCitation `json:"citations"`
	}
	if err := json.Unmarshal(result.StructuredContent, &structured); err != nil || len(structured.Activities) != 1 || len(structured.Citations) != 1 {
		t.Fatalf("structured=%#v err=%v", structured, err)
	}
}
