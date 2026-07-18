package config

import (
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
	user := Default(workspace)
	user.ActiveProvider = "work"
	user.Providers["work"] = validProvider("https://user.example/v1", "user-model")
	if err := Save(userPath, user); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(workspace, ".eylu", "config.toml")
	project := Default(workspace)
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
}

func TestConfigNeverContainsSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Default(t.TempDir())
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = validProvider("https://example.com/v1", "model")
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sk-test-secret") {
		t.Fatal("config persisted a credential value")
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
	cfg := Default(t.TempDir())
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

func validProvider(baseURL, model string) ProviderConfig {
	return ProviderConfig{
		Adapter: "openai_responses", BaseURL: baseURL, Model: model, TimeoutSeconds: 30,
		Credential: CredentialRef{Type: "env", Env: "EYLU_API_KEY"},
	}
}
