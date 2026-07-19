package app

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"Eylu/internal/config"
	"Eylu/internal/provider"
)

func TestResolveRuntimeAutomaticAndFixedRouting(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default(workspace)
	cfg.RoutingMode = "auto"
	cfg.ActiveProvider = "reasoning"
	cfg.Providers["reasoning"] = config.ProviderConfig{
		Adapter: "openai_responses", BaseURL: "https://reasoning.example/v1", Model: "reasoning-model", ContextWindow: 128_000,
		Credential: config.CredentialRef{Type: "none"}, Routing: config.ProviderRouting{Tasks: []string{"coding"}, InputCostPerMillion: 8},
	}
	cfg.Providers["cheap"] = config.ProviderConfig{
		Adapter: "openai_chat", BaseURL: "https://cheap.example/v1", Model: "cheap-model", ContextWindow: 64_000,
		Credential: config.CredentialRef{Type: "none"}, Routing: config.ProviderRouting{Tasks: []string{"coding"}, InputCostPerMillion: 1},
	}
	manager, err := provider.NewManager(filepath.Join(workspace, "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, credentials: provider.NewCredentialStore()}

	selected, decision, err := runtime.resolveRuntimeForPrompt(manager, chatOptions{}, "implement this", 10_000, 8_000, 2_000, true)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Provider.Name != "cheap" || decision == nil || decision.Task != "coding" {
		t.Fatalf("selected=%s decision=%#v", selected.Provider.Name, decision)
	}
	selected, decision, err = runtime.resolveRuntimeForPrompt(manager, chatOptions{requireReasoning: true}, "implement this", 10_000, 8_000, 2_000, true)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Provider.Name != "reasoning" || decision == nil {
		t.Fatalf("selected=%s decision=%#v", selected.Provider.Name, decision)
	}
	selected, decision, err = runtime.resolveRuntimeForPrompt(manager, chatOptions{provider: "reasoning"}, "implement this", 10_000, 8_000, 2_000, true)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Provider.Name != "reasoning" || decision != nil {
		t.Fatalf("fixed selection=%s decision=%#v", selected.Provider.Name, decision)
	}
}
