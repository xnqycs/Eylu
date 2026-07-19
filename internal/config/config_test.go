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
	cfg.Providers["work"] = validProvider("https://example.com/v1", "model")
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
	if strings.Contains(string(data), "workspace") {
		t.Fatalf("config persisted runtime workspace: %s", data)
	}
	loaded, err := Load(LoadOptions{ExplicitPath: path, Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.Providers["work"].APIKey != "sk-test-secret" {
		t.Fatal("provider API key did not round-trip")
	}
	encoded, err := json.Marshal(loaded.Config)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "sk-test-secret") {
		t.Fatal("provider API key leaked into JSON runtime state")
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
		"EYLU_TOKEN_BYTES_PER_TOKEN=3", "EYLU_RESERVED_OUTPUT_TOKENS=1024", "EYLU_CONTEXT_RECENT_ROUNDS=2", "EYLU_MAX_PROJECT_MAP_BYTES=4096", "EYLU_MAX_TOOL_CONTEXT_BYTES=2048", "EYLU_SKILL_CATALOG_PAGE_BYTES=1024", "EYLU_MAX_SUMMARY_BYTES=3072", "EYLU_MAX_SESSIONS=7", "EYLU_MAX_SESSION_BYTES=123456", "EYLU_ROUTING_MODE=auto", "EYLU_MAX_PARALLEL_TOOLS=6",
	}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := loaded.Config
	if cfg.TokenBytesPerToken != 3 || cfg.ReservedOutputTokens != 1024 || cfg.ContextRecentRounds != 2 || cfg.MaxProjectMapBytes != 4096 || cfg.MaxToolContextBytes != 2048 || cfg.SkillCatalogPageBytes != 1024 || cfg.MaxSummaryBytes != 3072 || cfg.MaxSessions != 7 || cfg.MaxSessionBytes != 123456 || cfg.RoutingMode != "auto" || cfg.MaxParallelTools != 6 {
		t.Fatalf("context config = %#v", cfg)
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
