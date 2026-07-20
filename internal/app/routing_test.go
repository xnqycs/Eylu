package app

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"Eylu/internal/config"
	"Eylu/internal/provider"
)

func TestResolveRuntimeAutomaticAndFixedRouting(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default()
	cfg.ModelMetadata.Enabled = false
	cfg.RoutingMode = "auto"
	cfg.ActiveProvider = "reasoning"
	cfg.Providers["reasoning"] = config.ProviderConfig{
		Adapter: "openai_responses", BaseURL: "https://reasoning.example/v1", Model: "reasoning-model", ContextWindow: 128_000,
		Routing: config.ProviderRouting{Tasks: []string{"coding"}, InputCostPerMillion: 8},
	}
	cfg.Providers["cheap"] = config.ProviderConfig{
		Adapter: "openai_chat", BaseURL: "https://cheap.example/v1", Model: "cheap-model", ContextWindow: 64_000,
		Routing: config.ProviderRouting{Tasks: []string{"coding"}, InputCostPerMillion: 1},
	}
	manager, err := provider.NewManager(filepath.Join(workspace, "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}

	selected, decision, err := runtime.resolveRuntimeForPrompt(context.Background(), manager, chatOptions{}, "implement this", 10_000, 8_000, 2_000, true)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Provider.Name != "cheap" || decision == nil || decision.Task != "coding" {
		t.Fatalf("selected=%s decision=%#v", selected.Provider.Name, decision)
	}
	selected, decision, err = runtime.resolveRuntimeForPrompt(context.Background(), manager, chatOptions{requireReasoning: true}, "implement this", 10_000, 8_000, 2_000, true)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Provider.Name != "reasoning" || decision == nil {
		t.Fatalf("selected=%s decision=%#v", selected.Provider.Name, decision)
	}
	selected, decision, err = runtime.resolveRuntimeForPrompt(context.Background(), manager, chatOptions{provider: "reasoning"}, "implement this", 10_000, 8_000, 2_000, true)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Provider.Name != "reasoning" || decision != nil {
		t.Fatalf("fixed selection=%s decision=%#v", selected.Provider.Name, decision)
	}
}

func TestContextLimitWarningIsEmittedOnce(t *testing.T) {
	var stderr bytes.Buffer
	runtime := &runtime{stderr: &stderr}
	snapshot := provider.Snapshot{Name: "work", Config: config.ProviderConfig{Model: "model", ContextWindow: 128000}}.WithLimits(provider.ModelLimits{ContextWindow: 64000, Source: provider.LimitSourceModelsDev})
	runtime.warnContextLimit(snapshot)
	runtime.warnContextLimit(snapshot)
	if strings.Count(stderr.String(), "configured override=128000 exceeds detected limit=64000; effective=128000") != 1 {
		t.Fatalf("warning = %q", stderr.String())
	}
}
