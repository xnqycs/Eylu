package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGradientEnabledDefaultsDisabledAndStorePersistsExplicitState(t *testing.T) {
	workspace := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.toml")
	loaded, err := Load(LoadOptions{ExplicitPath: configPath, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.GradientEnabled {
		t.Fatal("gradient should default to disabled")
	}
	updated, err := loaded.Store.SetGradientEnabled(true)
	if err != nil || !updated.GradientEnabled {
		t.Fatalf("enable config=%#v error=%v", updated, err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil || !strings.Contains(string(data), "gradient_enabled = true") {
		t.Fatalf("enabled config=%q error=%v", data, err)
	}
	updated, err = loaded.Store.SetGradientEnabled(false)
	if err != nil || updated.GradientEnabled {
		t.Fatalf("disable config=%#v error=%v", updated, err)
	}
	data, err = os.ReadFile(configPath)
	if err != nil || !strings.Contains(string(data), "gradient_enabled = false") {
		t.Fatalf("disabled config=%q error=%v", data, err)
	}
}

func TestLoadPrecedenceAndSave(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	userPath := filepath.Join(home, ".eylu", "config.toml")
	user := Default()
	user.ActiveProvider = "work"
	user.Providers["work"] = validProvider("https://user.example/v1", "user-model")
	if err := Save(userPath, user); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(workspace, ".eylu", "config.toml")
	project := Default()
	project.ActiveProvider = "work"
	project.Providers["work"] = validProvider("https://project.example/v1", "project-model")
	if err := Save(projectPath, project); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(LoadOptions{
		Workspace: workspace,
		Environ: []string{
			"EYLU_PROVIDER=work",
			"EYLU_BASE_URL=https://env.example/v1",
			"EYLU_MODEL=env-model",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.Config.Providers["work"]
	if got.BaseURL != "https://env.example/v1" || got.Model != "env-model" {
		t.Fatalf("environment override missing: %#v", got)
	}
	if loaded.Path != projectPath {
		t.Fatalf("write path = %s, want %s", loaded.Path, projectPath)
	}
	absoluteWorkspace, _ := filepath.Abs(workspace)
	if loaded.Workspace != absoluteWorkspace {
		t.Fatalf("workspace = %s, want %s", loaded.Workspace, absoluteWorkspace)
	}
}

func TestConfigPersistsProviderAPIKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Default()
	cfg.ActiveProvider = "work"
	provider := validProvider("https://example.com/v1", "model")
	provider.ReasoningEffort = "high"
	cfg.Providers["work"] = provider
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "api_key = 'sk-test-secret'") {
		t.Fatalf("config did not persist provider API key: %s", data)
	}
	if !strings.Contains(string(data), "reasoning_effort = 'high'") {
		t.Fatalf("config did not persist reasoning effort: %s", data)
	}
	if strings.Contains(string(data), "workspace") {
		t.Fatalf("config persisted runtime workspace: %s", data)
	}
	loaded, err := Load(LoadOptions{ExplicitPath: path, Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Config.Providers["work"]; got.APIKey != "sk-test-secret" || got.ReasoningEffort != "high" {
		t.Fatalf("provider settings did not round-trip: %#v", got)
	}
	encoded, err := json.Marshal(loaded.Config)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "sk-test-secret") {
		t.Fatal("provider API key leaked into JSON runtime state")
	}
}

func TestReasoningEffortProfilesAndValidation(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{model: "openai/gpt-5.6-sol", want: "auto,low,medium,high,xhigh,max,ultra"},
		{model: "gpt-5.4-pro", want: "auto,high"},
		{model: "gpt-5.4", want: "auto,low,medium,high,xhigh"},
		{model: "gpt-5.1-codex", want: "auto,low,medium,high"},
		{model: "anthropic/claude-opus-4.7", want: "auto,low,medium,high,xhigh,max"},
		{model: "claude-opus-5.1", want: "auto,low,medium,high,xhigh,max"},
		{model: "claude-sonnet-4.6", want: "auto,high,max"},
		{model: "google/gemini-3-pro", want: "auto,low,high"},
		{model: "deepseek-v4", want: "auto,high,max"},
		{model: "glm-5.2", want: "auto,high,max"},
		{model: "kimi-k3", want: "auto,max"},
		{model: "qwen3", want: "auto"},
		{model: "custom-model", want: "auto,low,medium,high"},
	}
	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			if got := strings.Join(SupportedReasoningEfforts(tc.model), ","); got != tc.want {
				t.Fatalf("SupportedReasoningEfforts(%q) = %q, want %q", tc.model, got, tc.want)
			}
		})
	}

	if got := EffectiveReasoningEffort(""); got != "auto" {
		t.Fatalf("EffectiveReasoningEffort(empty) = %q", got)
	}
	if err := ValidateReasoningEffort("gpt-5.6-sol", "ultra"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateReasoningEffort("qwen3", "high"); err == nil || !strings.Contains(err.Error(), "available: auto") {
		t.Fatalf("incompatible effort error = %v", err)
	}
	if err := ValidateReasoningEffort("gpt-5.6-sol", "extreme"); err == nil {
		t.Fatal("invalid effort passed validation")
	}
}

func TestProviderReasoningEffortPatch(t *testing.T) {
	provider := validProvider("https://example.com/v1", "gpt-5.6-sol")
	provider = ApplyProviderPatch(provider, ProviderPatch{ReasoningEffort: SetValue("max")})
	if provider.ReasoningEffort != "max" || SparseProviderPatch(provider).ReasoningEffort.Value != "max" || !SparseProviderPatch(provider).ReasoningEffort.Set {
		t.Fatalf("reasoning effort patch was not preserved: %#v", provider)
	}
}

func TestLegacyWorkspaceIsIgnored(t *testing.T) {
	workspace := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.toml")
	data := []byte("version = 1\nworkspace = \"C:/legacy\"\n")
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(LoadOptions{ExplicitPath: configPath, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	absoluteWorkspace, _ := filepath.Abs(workspace)
	if loaded.Workspace != absoluteWorkspace {
		t.Fatalf("legacy workspace changed runtime workspace: %s", loaded.Workspace)
	}
	if err := Save(configPath, loaded.Config); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "workspace") {
		t.Fatalf("legacy workspace survived save: %s", saved)
	}
}

func TestValidateProvider(t *testing.T) {
	tests := []struct {
		name string
		cfg  ProviderConfig
	}{
		{name: "missing adapter", cfg: ProviderConfig{BaseURL: "https://example.com/v1", Model: "m"}},
		{name: "relative URL", cfg: ProviderConfig{Adapter: "a", BaseURL: "/v1", Model: "m"}},
		{name: "query URL", cfg: ProviderConfig{Adapter: "a", BaseURL: "https://example.com/v1?q=1", Model: "m"}},
		{name: "missing model", cfg: ProviderConfig{Adapter: "a", BaseURL: "https://example.com/v1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateProvider("work", tc.cfg); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestValidatePermissionModeAndClonePolicyLists(t *testing.T) {
	cfg := Default()
	clone := cfg.Clone()
	clone.AutoAllowCommands[0] = "changed"
	if cfg.AutoAllowCommands[0] == "changed" {
		t.Fatal("Clone shares policy command slices")
	}
	cfg.PermissionMode = "unsafe"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid permission mode error")
	}
}

func TestContextLimitEnvironmentOverrides(t *testing.T) {
	workspace := t.TempDir()
	loaded, err := Load(LoadOptions{ExplicitPath: filepath.Join(t.TempDir(), "missing.toml"), Workspace: workspace, Environ: []string{
		"EYLU_TOKEN_BYTES_PER_TOKEN=3", "EYLU_RESERVED_OUTPUT_TOKENS=1024", "EYLU_CONTEXT_RECENT_ROUNDS=2", "EYLU_CONTEXT_COMPACT_TRIGGER_PERCENT=80", "EYLU_CONTEXT_COMPACT_TARGET_PERCENT=55", "EYLU_MAX_PROJECT_MAP_BYTES=4096", "EYLU_MAX_TOOL_CONTEXT_BYTES=2048", "EYLU_SKILL_CATALOG_PAGE_BYTES=1024", "EYLU_MAX_SUMMARY_BYTES=3072", "EYLU_MAX_SESSIONS=7", "EYLU_MAX_SESSION_BYTES=123456", "EYLU_ROUTING_MODE=auto", "EYLU_MAX_PARALLEL_TOOLS=6", "EYLU_MODEL_METADATA_ENABLED=false",
	}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := loaded.Config
	if cfg.TokenBytesPerToken != 3 || cfg.ReservedOutputTokens != 1024 || cfg.ContextRecentRounds != 2 || cfg.ContextCompactTrigger != 80 || cfg.ContextCompactTarget != 55 || cfg.MaxProjectMapBytes != 4096 || cfg.MaxToolContextBytes != 2048 || cfg.SkillCatalogPageBytes != 1024 || cfg.MaxSummaryBytes != 3072 || cfg.MaxSessions != 7 || cfg.MaxSessionBytes != 123456 || cfg.RoutingMode != "auto" || cfg.MaxParallelTools != 6 || cfg.ModelMetadata.Enabled {
		t.Fatalf("context config = %#v", cfg)
	}
}

func TestContextCompactionPercentValidation(t *testing.T) {
	for _, values := range [][2]int{{0, 60}, {100, 60}, {60, 60}, {50, 60}} {
		cfg := Default()
		cfg.ContextCompactTrigger = values[0]
		cfg.ContextCompactTarget = values[1]
		if err := cfg.Validate(); err == nil {
			t.Fatalf("trigger=%d target=%d accepted", values[0], values[1])
		}
	}
	cfg := Default()
	cfg.ContextCompactTrigger = 85
	cfg.ContextCompactTarget = 60
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestContextCompactionPercentagesPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Default()
	cfg.ContextCompactTrigger = 80
	cfg.ContextCompactTarget = 50
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(LoadOptions{ExplicitPath: path, Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.ContextCompactTrigger != 80 || loaded.Config.ContextCompactTarget != 50 {
		t.Fatalf("config=%#v", loaded.Config)
	}
}

func TestProviderRoutingValidationAndClone(t *testing.T) {
	cfg := Default()
	cfg.ActiveProvider = "routed"
	cfg.RoutingMode = "auto"
	cfg.Providers["routed"] = ProviderConfig{
		Adapter: "openai_responses", BaseURL: "https://example.test/v1", Model: "model",
		Routing: ProviderRouting{Tasks: []string{"coding", "testing"}, Priority: 2, InputCostPerMillion: 1.25, OutputCostPerMillion: 4.5},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	clone := cfg.Clone()
	clone.Providers["routed"].Routing.Tasks[0] = "review"
	if cfg.Providers["routed"].Routing.Tasks[0] != "coding" {
		t.Fatal("routing task slice was shared by Clone")
	}
	candidate := cfg.Clone()
	provider := candidate.Providers["routed"]
	provider.Routing.Tasks = []string{"unknown"}
	candidate.Providers["routed"] = provider
	if err := candidate.Validate(); err == nil || !strings.Contains(err.Error(), "routing task") {
		t.Fatalf("error = %v", err)
	}
	candidate = cfg.Clone()
	candidate.RoutingMode = "random"
	if err := candidate.Validate(); err == nil || !strings.Contains(err.Error(), "routing_mode") {
		t.Fatalf("error = %v", err)
	}
}

func TestMCPServerValidationAndClone(t *testing.T) {
	cfg := Default()
	cfg.MCPServers["workspace"] = MCPServerConfig{
		Command: "mcp-server", Args: []string{"--stdio"}, Environment: []string{"MCP_TOKEN"},
		WorkingDirectory: ".", ReadOnlyTools: []string{"search"}, TimeoutSeconds: 15,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	clone := cfg.Clone()
	server := clone.MCPServers["workspace"]
	server.Args[0], server.Environment[0], server.ReadOnlyTools[0] = "changed", "OTHER", "other"
	if cfg.MCPServers["workspace"].Args[0] != "--stdio" || cfg.MCPServers["workspace"].Environment[0] != "MCP_TOKEN" || cfg.MCPServers["workspace"].ReadOnlyTools[0] != "search" {
		t.Fatal("MCP server slices were shared by Clone")
	}
	for name, candidate := range map[string]MCPServerConfig{
		"invalid name": {Command: "server"},
		"missing":      {},
		"secret":       {Command: "server", Environment: []string{"TOKEN=value"}},
	} {
		invalid := Default()
		invalid.MCPServers[name] = candidate
		if err := invalid.Validate(); err == nil {
			t.Fatalf("server %q passed validation", name)
		}
	}
}

func TestSkillRegistryValidationAndClone(t *testing.T) {
	publicKey := base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
	cfg := Default()
	cfg.SkillRegistries["team"] = SkillRegistryConfig{
		IndexURL: "https://registry.example/index.json", PublicKeys: map[string]string{"release": publicKey},
		TokenEnvironment: "REGISTRY_TOKEN", TimeoutSeconds: 30,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	clone := cfg.Clone()
	clone.SkillRegistries["team"].PublicKeys["release"] = "changed"
	if cfg.SkillRegistries["team"].PublicKeys["release"] != publicKey {
		t.Fatal("Skill registry public key map was shared by Clone")
	}
	for name, candidate := range map[string]SkillRegistryConfig{
		"insecure": {IndexURL: "http://example.com/index.json", PublicKeys: map[string]string{"release": publicKey}},
		"secret":   {IndexURL: "https://example.com/index.json", PublicKeys: map[string]string{"release": publicKey}, TokenEnvironment: "TOKEN=value"},
		"key":      {IndexURL: "https://example.com/index.json", PublicKeys: map[string]string{"release": "invalid"}},
	} {
		invalid := Default()
		invalid.SkillRegistries[name] = candidate
		if err := invalid.Validate(); err == nil {
			t.Fatalf("registry %s passed validation", name)
		}
	}
}

func validProvider(baseURL, model string) ProviderConfig {
	return ProviderConfig{
		Adapter: "openai_responses", BaseURL: baseURL, APIKey: "sk-test-secret", Model: model, TimeoutSeconds: 30,
	}
}

func TestSaveOmitsBuiltInDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(path, Default()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.TrimSpace(text) != "version = 1" {
		t.Fatalf("default config was expanded: %s", text)
	}
}

func TestSaveProviderOmitsOptionalDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Default()
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = ProviderConfig{Adapter: "openai_chat", BaseURL: "https://example.com/v1", Model: "model"}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, unexpected := range []string{"model_metadata", "timeout_seconds", "context_window", "api_key", "max_turns"} {
		if strings.Contains(text, unexpected) {
			t.Fatalf("optional default %q was persisted: %s", unexpected, text)
		}
	}
}

func TestStorePreservesExplicitValuesEqualToDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := []byte("version = 1\nmax_turns = 20\n\n[model_metadata]\nenabled = true\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(LoadOptions{ExplicitPath: path, Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loaded.Store.SetActiveProvider(""); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(saved)
	for _, expected := range []string{"max_turns = 20", "[model_metadata]", "enabled = true"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("explicit field %q was lost: %s", expected, text)
		}
	}
}

func TestStorePreservesExplicitFalseZeroAndEmptyList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := []byte("version = 1\nactive_provider = 'work'\nblocked_commands = []\n\n[model_metadata]\nenabled = false\n\n[providers.work]\nadapter = 'openai_chat'\nbase_url = 'https://example.com/v1'\nmodel = 'model'\ncontext_window = 0\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(LoadOptions{ExplicitPath: path, Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.ModelMetadata.Enabled || loaded.Config.Providers["work"].ContextWindow != 0 || len(loaded.Config.BlockedCommands) != 0 {
		t.Fatalf("effective config = %#v", loaded.Config)
	}
	if _, err := loaded.Store.SetActiveProvider("work"); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(saved)
	for _, expected := range []string{"blocked_commands = []", "enabled = false", "context_window = 0"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("explicit field %q was lost: %s", expected, text)
		}
	}
}

func TestStoreUpdatesOnlyCurrentLayer(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	userPath := filepath.Join(home, ".eylu", "config.toml")
	projectPath := filepath.Join(workspace, ".eylu", "config.toml")
	if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
		t.Fatal(err)
	}
	user := "version = 1\nactive_provider = 'work'\n\n[providers.work]\nadapter = 'openai_responses'\nbase_url = 'https://example.com/v1'\napi_key = 'secret'\nmodel = 'base-model'\n"
	if err := os.WriteFile(userPath, []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte("version = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(LoadOptions{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loaded.Store.UpdateProvider("work", ProviderPatch{Model: SetValue("project-model")}, false); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(saved)
	if !strings.Contains(text, "model = 'project-model'") || strings.Contains(text, "base_url") || strings.Contains(text, "api_key") {
		t.Fatalf("project layer was flattened: %s", text)
	}
	reloaded, err := Load(LoadOptions{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	provider := reloaded.Config.Providers["work"]
	if provider.Model != "project-model" || provider.BaseURL != "https://example.com/v1" || provider.APIKey != "secret" {
		t.Fatalf("layered provider = %#v", provider)
	}
	if _, err := reloaded.Store.DeleteProvider("work", ""); err != nil {
		t.Fatal(err)
	}
	reloaded, err = Load(LoadOptions{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := reloaded.Config.Providers["work"]; exists {
		t.Fatal("removed provider resurfaced from the user layer")
	}
}

func TestStoreDoesNotPersistEnvironmentOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := "version = 1\nactive_provider = 'work'\n\n[providers.work]\nadapter = 'openai_responses'\nbase_url = 'https://example.com/v1'\nmodel = 'file-model'\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(LoadOptions{ExplicitPath: path, Workspace: t.TempDir(), Environ: []string{"EYLU_MODEL=env-model"}})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.Providers["work"].Model != "env-model" {
		t.Fatal("environment override was not applied")
	}
	if _, err := loaded.Store.SetActiveProvider("work"); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "env-model") {
		t.Fatalf("environment override was persisted: %s", saved)
	}
}
