package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"Eylu/internal/config"
)

func TestLimitResolverReadsOpenAICompatibleMetadataAndCache(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "model-a", "context_length": 131072, "max_completion_tokens": 16384}}})
	}))
	defer server.Close()
	resolver := NewLimitResolver(config.Default().ModelMetadata, filepath.Join(t.TempDir(), "cache.json"), server.Client())
	snapshot := Snapshot{Name: "work", Config: config.ProviderConfig{Adapter: "openai_chat", BaseURL: server.URL + "/v1", Model: "model-a", ContextWindow: 64000}}
	resolved, err := resolver.Resolve(context.Background(), snapshot, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Limits.ContextWindow != 131072 || resolved.Limits.MaxOutputTokens != 16384 || resolved.EffectiveContextWindow != 64000 || resolved.Limits.Source != LimitSourceOpenAIExtension {
		t.Fatalf("resolved limits = %#v", resolved)
	}
	second, err := resolver.Resolve(context.Background(), snapshot, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Limits.Cached || requests.Load() != 1 {
		t.Fatalf("cache miss: requests=%d limits=%#v", requests.Load(), second.Limits)
	}
}

func TestLimitResolverReadsOllamaAndLlamaCPP(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		source  LimitSource
		window  int
	}{
		{name: "ollama ps", source: LimitSourceOllamaPS, window: 32768, handler: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/ps" {
				_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{"name": "llama:latest", "context_length": 32768}}})
				return
			}
			http.NotFound(w, r)
		}},
		{name: "llama cpp", source: LimitSourceLlamaCPP, window: 65536, handler: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/props" {
				_ = json.NewEncoder(w).Encode(map[string]any{"default_generation_settings": map[string]any{"n_ctx": 65536}})
				return
			}
			http.NotFound(w, r)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			metadata := config.Default().ModelMetadata
			metadata.CatalogURL = server.URL + "/catalog"
			resolver := NewLimitResolver(metadata, filepath.Join(t.TempDir(), "cache.json"), server.Client())
			resolved, err := resolver.Resolve(context.Background(), Snapshot{Name: "local", Config: config.ProviderConfig{Adapter: "openai_chat", BaseURL: server.URL + "/v1", Model: "llama"}}, "")
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Limits.ContextWindow != tc.window || resolved.Limits.Source != tc.source {
				t.Fatalf("limits = %#v", resolved.Limits)
			}
		})
	}
}

func TestLimitResolverMapsModelsDevAndLearnsOverflow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/catalog" {
			_ = json.NewEncoder(w).Encode(map[string]any{"openai": map[string]any{"id": "openai", "models": map[string]any{"gpt-test": map[string]any{"id": "gpt-test", "limit": map[string]any{"context": 200000, "output": 32000}}}}})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	metadata := config.Default().ModelMetadata
	metadata.CatalogURL = server.URL + "/catalog"
	resolver := NewLimitResolver(metadata, filepath.Join(t.TempDir(), "cache.json"), server.Client())
	snapshot := Snapshot{Name: "proxy", Config: config.ProviderConfig{Adapter: "openai_responses", BaseURL: server.URL + "/v1", Model: "openai/gpt-test", CatalogProvider: "openai"}}
	resolved, err := resolver.Resolve(context.Background(), snapshot, "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Limits.Source != LimitSourceModelsDev || resolved.Limits.ContextWindow != 200000 || resolved.Limits.MaxOutputTokens != 32000 {
		t.Fatalf("catalog limits = %#v", resolved.Limits)
	}
	learned := resolver.LearnOverflow(resolved, 0)
	if learned.Limits.Source != LimitSourceOverflow || learned.EffectiveContextWindow != 128000 || learned.Limits.Degradations != 1 {
		t.Fatalf("learned limits = %#v", learned)
	}
	cached, err := resolver.Resolve(context.Background(), snapshot, "")
	if err != nil {
		t.Fatal(err)
	}
	if cached.Limits.Source != LimitSourceOverflow || !cached.Limits.Cached || cached.EffectiveContextWindow != 128000 {
		t.Fatalf("learned cache = %#v", cached)
	}
}

func TestLimitResolverUsesFallbackWhenRemoteLookupDisabled(t *testing.T) {
	metadata := config.Default().ModelMetadata
	metadata.Enabled = false
	resolver := NewLimitResolver(metadata, filepath.Join(t.TempDir(), "cache.json"), nil)
	resolved, err := resolver.Resolve(context.Background(), Snapshot{Name: "offline", Config: config.ProviderConfig{BaseURL: "https://offline.example/v1", Model: "unknown"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Limits.Source != LimitSourceFallback || !resolved.Limits.Assumed || resolved.EffectiveContextWindow != 256000 {
		t.Fatalf("fallback = %#v", resolved)
	}
}

func TestLimitResolverRebuildsCorruptCache(t *testing.T) {
	metadata := config.Default().ModelMetadata
	metadata.Enabled = false
	path := filepath.Join(t.TempDir(), "cache.json")
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver := NewLimitResolver(metadata, path, nil)
	if _, err := resolver.Resolve(context.Background(), Snapshot{Name: "offline", Config: config.ProviderConfig{BaseURL: "https://offline.example/v1", Model: "unknown"}}, ""); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !json.Valid(raw) {
		t.Fatalf("cache=%q err=%v", raw, err)
	}
}
