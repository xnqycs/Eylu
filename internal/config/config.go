package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const SchemaVersion = 1

type CredentialRef struct {
	Type    string `toml:"type" json:"type"`
	Service string `toml:"service,omitempty" json:"service,omitempty"`
	Account string `toml:"account,omitempty" json:"account,omitempty"`
	Env     string `toml:"env,omitempty" json:"env,omitempty"`
}

type ProviderConfig struct {
	Adapter        string            `toml:"adapter" json:"adapter"`
	BaseURL        string            `toml:"base_url" json:"base_url"`
	Model          string            `toml:"model" json:"model"`
	ContextWindow  int               `toml:"context_window,omitempty" json:"context_window,omitempty"`
	TimeoutSeconds int               `toml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
	Credential     CredentialRef     `toml:"credential" json:"credential"`
	Headers        map[string]string `toml:"headers,omitempty" json:"headers,omitempty"`
}

func (p ProviderConfig) Timeout(fallback time.Duration) time.Duration {
	if p.TimeoutSeconds <= 0 {
		return fallback
	}
	return time.Duration(p.TimeoutSeconds) * time.Second
}

type Config struct {
	Version           int                       `toml:"version" json:"version"`
	ActiveProvider    string                    `toml:"active_provider" json:"active_provider"`
	Providers         map[string]ProviderConfig `toml:"providers" json:"providers"`
	Workspace         string                    `toml:"workspace,omitempty" json:"workspace,omitempty"`
	PermissionMode    string                    `toml:"permission_mode,omitempty" json:"permission_mode,omitempty"`
	MaxTurns          int                       `toml:"max_turns,omitempty" json:"max_turns,omitempty"`
	MaxTotalTokens    int                       `toml:"max_total_tokens,omitempty" json:"max_total_tokens,omitempty"`
	ToolTimeoutSec    int                       `toml:"tool_timeout_seconds,omitempty" json:"tool_timeout_seconds,omitempty"`
	MaxOutputBytes    int                       `toml:"max_output_bytes,omitempty" json:"max_output_bytes,omitempty"`
	MaxReadBytes      int                       `toml:"max_read_bytes,omitempty" json:"max_read_bytes,omitempty"`
	MaxSearchResults  int                       `toml:"max_search_results,omitempty" json:"max_search_results,omitempty"`
	ReadOnlyCommands  []string                  `toml:"read_only_commands,omitempty" json:"read_only_commands,omitempty"`
	AutoAllowCommands []string                  `toml:"auto_allow_commands,omitempty" json:"auto_allow_commands,omitempty"`
	DangerousCommands []string                  `toml:"dangerous_commands,omitempty" json:"dangerous_commands,omitempty"`
	BlockedCommands   []string                  `toml:"blocked_commands,omitempty" json:"blocked_commands,omitempty"`
	ShellEnvironment  []string                  `toml:"shell_environment,omitempty" json:"shell_environment,omitempty"`
}

func Default(workspace string) Config {
	return Config{
		Version:           SchemaVersion,
		Providers:         make(map[string]ProviderConfig),
		Workspace:         workspace,
		PermissionMode:    "manual",
		MaxTurns:          20,
		MaxTotalTokens:    1_000_000,
		ToolTimeoutSec:    60,
		MaxOutputBytes:    64 << 10,
		MaxReadBytes:      1 << 20,
		MaxSearchResults:  200,
		ReadOnlyCommands:  []string{"ls", "dir", "pwd", "find", "rg", "grep", "git status", "git diff", "git log", "git show", "git grep", "git branch", "git rev-parse", "git ls-files"},
		AutoAllowCommands: []string{"ls", "dir", "pwd", "find", "rg", "grep", "git status", "git diff", "git log", "git show", "git grep", "git branch", "git rev-parse", "git ls-files", "go test", "go vet", "go build", "go list", "go env", "go version", "gofmt", "go fmt"},
		DangerousCommands: []string{"rm -rf", "git reset --hard", "git clean -fd", "git push --force", "mkfs", "diskpart", "format ", "remove-item -recurse", "del /s", "rd /s"},
	}
}

func (c Config) Clone() Config {
	clone := c
	clone.ReadOnlyCommands = append([]string(nil), c.ReadOnlyCommands...)
	clone.AutoAllowCommands = append([]string(nil), c.AutoAllowCommands...)
	clone.DangerousCommands = append([]string(nil), c.DangerousCommands...)
	clone.BlockedCommands = append([]string(nil), c.BlockedCommands...)
	clone.ShellEnvironment = append([]string(nil), c.ShellEnvironment...)
	clone.Providers = make(map[string]ProviderConfig, len(c.Providers))
	for name, provider := range c.Providers {
		provider.Headers = cloneStringMap(provider.Headers)
		clone.Providers[name] = provider
	}
	return clone
}

func (c Config) Validate() error {
	if c.Version != SchemaVersion {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	if c.ActiveProvider != "" {
		if _, ok := c.Providers[c.ActiveProvider]; !ok {
			return fmt.Errorf("active provider %q does not exist", c.ActiveProvider)
		}
	}
	for name, provider := range c.Providers {
		if err := ValidateProvider(name, provider); err != nil {
			return err
		}
	}
	if c.MaxTurns <= 0 || c.MaxTotalTokens <= 0 {
		return errors.New("turn and token budgets must be greater than zero")
	}
	if c.ToolTimeoutSec <= 0 || c.MaxOutputBytes <= 0 || c.MaxReadBytes <= 0 || c.MaxSearchResults <= 0 {
		return errors.New("resource limits must be greater than zero")
	}
	switch c.PermissionMode {
	case "manual", "plan", "auto", "full":
	default:
		return fmt.Errorf("invalid permission_mode %q", c.PermissionMode)
	}
	return nil
}

func ValidateProvider(name string, p ProviderConfig) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("provider name is required")
	}
	if strings.TrimSpace(p.Adapter) == "" {
		return fmt.Errorf("provider %q adapter is required", name)
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("provider %q base_url must be an absolute http(s) URL without query or fragment", name)
	}
	if strings.TrimSpace(p.Model) == "" {
		return fmt.Errorf("provider %q model is required", name)
	}
	if p.ContextWindow < 0 || p.TimeoutSeconds < 0 {
		return fmt.Errorf("provider %q numeric limits cannot be negative", name)
	}
	switch p.Credential.Type {
	case "", "keyring", "env", "memory", "none":
	default:
		return fmt.Errorf("provider %q credential type %q is invalid", name, p.Credential.Type)
	}
	if p.Credential.Type == "env" && p.Credential.Env == "" {
		return fmt.Errorf("provider %q credential env is required", name)
	}
	return nil
}

type LoadOptions struct {
	ExplicitPath string
	Workspace    string
	Environ      []string
}

type Loaded struct {
	Config Config
	Path   string
}

func Load(opts LoadOptions) (Loaded, error) {
	workspace, err := filepath.Abs(opts.Workspace)
	if err != nil {
		return Loaded{}, fmt.Errorf("resolve workspace: %w", err)
	}
	result := Default(workspace)
	paths := configPaths(opts.ExplicitPath, workspace)
	writePath := opts.ExplicitPath
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return Loaded{}, fmt.Errorf("read config %s: %w", path, readErr)
		}
		var overlay Config
		if err := toml.Unmarshal(data, &overlay); err != nil {
			return Loaded{}, fmt.Errorf("parse config %s: %w", path, err)
		}
		merge(&result, overlay)
		writePath = path
	}
	if writePath == "" {
		writePath = defaultUserConfigPath()
	}
	applyEnvironment(&result, opts.Environ)
	if result.Workspace == "" {
		result.Workspace = workspace
	}
	if err := result.Validate(); err != nil {
		return Loaded{}, err
	}
	return Loaded{Config: result, Path: writePath}, nil
}

func configPaths(explicit, workspace string) []string {
	if explicit != "" {
		return []string{explicit}
	}
	return []string{defaultUserConfigPath(), filepath.Join(workspace, ".eylu", "config.toml")}
}

func defaultUserConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".eylu", "config.toml")
	}
	return filepath.Join(home, ".eylu", "config.toml")
}

func Save(path string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temporary config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceFile(tmpName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

func merge(dst *Config, src Config) {
	if src.Version != 0 {
		dst.Version = src.Version
	}
	if src.ActiveProvider != "" {
		dst.ActiveProvider = src.ActiveProvider
	}
	if src.Providers != nil {
		for name, provider := range src.Providers {
			dst.Providers[name] = provider
		}
	}
	if src.Workspace != "" {
		dst.Workspace = src.Workspace
	}
	if src.PermissionMode != "" {
		dst.PermissionMode = src.PermissionMode
	}
	if src.MaxTurns != 0 {
		dst.MaxTurns = src.MaxTurns
	}
	if src.MaxTotalTokens != 0 {
		dst.MaxTotalTokens = src.MaxTotalTokens
	}
	if src.ToolTimeoutSec != 0 {
		dst.ToolTimeoutSec = src.ToolTimeoutSec
	}
	if src.MaxOutputBytes != 0 {
		dst.MaxOutputBytes = src.MaxOutputBytes
	}
	if src.MaxReadBytes != 0 {
		dst.MaxReadBytes = src.MaxReadBytes
	}
	if src.MaxSearchResults != 0 {
		dst.MaxSearchResults = src.MaxSearchResults
	}
	if src.ReadOnlyCommands != nil {
		dst.ReadOnlyCommands = append([]string(nil), src.ReadOnlyCommands...)
	}
	if src.AutoAllowCommands != nil {
		dst.AutoAllowCommands = append([]string(nil), src.AutoAllowCommands...)
	}
	if src.DangerousCommands != nil {
		dst.DangerousCommands = append([]string(nil), src.DangerousCommands...)
	}
	if src.BlockedCommands != nil {
		dst.BlockedCommands = append([]string(nil), src.BlockedCommands...)
	}
	if src.ShellEnvironment != nil {
		dst.ShellEnvironment = append([]string(nil), src.ShellEnvironment...)
	}
}

func applyEnvironment(cfg *Config, environ []string) {
	env := make(map[string]string, len(environ))
	for _, item := range environ {
		if key, value, ok := strings.Cut(item, "="); ok {
			env[key] = value
		}
	}
	if value := env["EYLU_PROVIDER"]; value != "" {
		cfg.ActiveProvider = value
	}
	if value := env["EYLU_WORKSPACE"]; value != "" {
		cfg.Workspace = value
	}
	if value := env["EYLU_PERMISSION_MODE"]; value != "" {
		cfg.PermissionMode = value
	}
	for key, target := range map[string]*int{
		"EYLU_MAX_TURNS":        &cfg.MaxTurns,
		"EYLU_MAX_TOTAL_TOKENS": &cfg.MaxTotalTokens,
		"EYLU_TOOL_TIMEOUT":     &cfg.ToolTimeoutSec,
		"EYLU_MAX_OUTPUT_BYTES": &cfg.MaxOutputBytes,
	} {
		if value := env[key]; value != "" {
			if parsed, err := strconv.Atoi(value); err == nil {
				*target = parsed
			}
		}
	}
	if cfg.ActiveProvider != "" {
		provider, ok := cfg.Providers[cfg.ActiveProvider]
		if ok {
			if value := env["EYLU_BASE_URL"]; value != "" {
				provider.BaseURL = value
			}
			if value := env["EYLU_MODEL"]; value != "" {
				provider.Model = value
			}
			cfg.Providers[cfg.ActiveProvider] = provider
		}
	}
}

func ProviderNames(cfg Config) []string {
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func cloneStringMap(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
