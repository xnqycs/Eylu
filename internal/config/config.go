package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	SchemaVersion       = 1
	ReasoningEffortAuto = "auto"
)

var allReasoningEfforts = []string{"auto", "low", "medium", "high", "xhigh", "max", "ultra"}

type ProviderConfig struct {
	Adapter         string            `toml:"adapter" json:"adapter"`
	BaseURL         string            `toml:"base_url" json:"base_url"`
	APIKey          string            `toml:"api_key,omitempty" json:"-"`
	Model           string            `toml:"model" json:"model"`
	ReasoningEffort string            `toml:"reasoning_effort,omitempty" json:"reasoning_effort,omitempty"`
	CatalogProvider string            `toml:"catalog_provider,omitempty" json:"catalog_provider,omitempty"`
	ContextWindow   int               `toml:"context_window,omitempty" json:"context_window,omitempty"`
	TimeoutSeconds  int               `toml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
	Headers         map[string]string `toml:"headers,omitempty" json:"headers,omitempty"`
	Routing         ProviderRouting   `toml:"routing,omitempty" json:"routing,omitempty"`
}

type ModelMetadataConfig struct {
	Enabled               bool   `toml:"enabled" json:"enabled"`
	CatalogURL            string `toml:"catalog_url" json:"catalog_url"`
	RequestTimeoutSeconds int    `toml:"request_timeout_seconds" json:"request_timeout_seconds"`
	EndpointTTLHours      int    `toml:"endpoint_ttl_hours" json:"endpoint_ttl_hours"`
	CatalogTTLHours       int    `toml:"catalog_ttl_hours" json:"catalog_ttl_hours"`
	StaleTTLHours         int    `toml:"stale_ttl_hours" json:"stale_ttl_hours"`
	NegativeTTLMinutes    int    `toml:"negative_ttl_minutes" json:"negative_ttl_minutes"`
	LearnedTTLHours       int    `toml:"learned_ttl_hours" json:"learned_ttl_hours"`
	MaxResponseBytes      int    `toml:"max_response_bytes" json:"max_response_bytes"`
	MaxCacheEntries       int    `toml:"max_cache_entries" json:"max_cache_entries"`
	ProbeTiers            []int  `toml:"probe_tiers" json:"probe_tiers"`
}

type ProviderRouting struct {
	Tasks                []string `toml:"tasks,omitempty" json:"tasks,omitempty"`
	Priority             int      `toml:"priority,omitempty" json:"priority,omitempty"`
	InputCostPerMillion  float64  `toml:"input_cost_per_million,omitempty" json:"input_cost_per_million,omitempty"`
	OutputCostPerMillion float64  `toml:"output_cost_per_million,omitempty" json:"output_cost_per_million,omitempty"`
}

type MCPServerConfig struct {
	Command          string   `toml:"command" json:"command"`
	Args             []string `toml:"args,omitempty" json:"args,omitempty"`
	Environment      []string `toml:"environment,omitempty" json:"environment,omitempty"`
	WorkingDirectory string   `toml:"working_directory,omitempty" json:"working_directory,omitempty"`
	ReadOnlyTools    []string `toml:"read_only_tools,omitempty" json:"read_only_tools,omitempty"`
	TimeoutSeconds   int      `toml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
	Disabled         bool     `toml:"disabled,omitempty" json:"disabled,omitempty"`
}

type SkillRegistryConfig struct {
	IndexURL         string            `toml:"index_url" json:"index_url"`
	PublicKeys       map[string]string `toml:"public_keys" json:"public_keys"`
	TokenEnvironment string            `toml:"token_environment,omitempty" json:"token_environment,omitempty"`
	TimeoutSeconds   int               `toml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
	Disabled         bool              `toml:"disabled,omitempty" json:"disabled,omitempty"`
}

func (p ProviderConfig) Timeout(fallback time.Duration) time.Duration {
	if p.TimeoutSeconds <= 0 {
		return fallback
	}
	return time.Duration(p.TimeoutSeconds) * time.Second
}

func EffectiveReasoningEffort(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ReasoningEffortAuto
	}
	return value
}

func SupportedReasoningEfforts(modelID string) []string {
	model := normalizedModelID(modelID)
	levels := []string{"auto", "low", "medium", "high"}
	switch {
	case strings.HasPrefix(model, "gpt-5.6-sol"), strings.HasPrefix(model, "gpt-5.6-terra"):
		levels = allReasoningEfforts
	case isOpenAIProModel(model):
		levels = []string{"auto", "high"}
	case model == "gpt-5.1-codex-max", strings.HasPrefix(model, "gpt-5.1-codex-max-"), modernGPTXHigh(model):
		levels = []string{"auto", "low", "medium", "high", "xhigh"}
	case strings.HasPrefix(model, "gpt-5"), strings.HasPrefix(model, "codex"), strings.HasPrefix(model, "o1"), strings.HasPrefix(model, "o3"), strings.HasPrefix(model, "o4"), strings.HasPrefix(model, "gpt-oss"):
		levels = []string{"auto", "low", "medium", "high"}
	case modernClaudeOpus(model):
		levels = []string{"auto", "low", "medium", "high", "xhigh", "max"}
	case strings.HasPrefix(model, "claude-"):
		levels = []string{"auto", "high", "max"}
	case strings.HasPrefix(model, "gemini-"):
		levels = []string{"auto", "low", "high"}
	case strings.HasPrefix(model, "deepseek-v4"), strings.HasPrefix(model, "glm-5.2"):
		levels = []string{"auto", "high", "max"}
	case strings.HasPrefix(model, "kimi-k3"):
		levels = []string{"auto", "max"}
	case strings.HasPrefix(model, "qwen"), strings.HasPrefix(model, "glm-"), strings.HasPrefix(model, "kimi-"), strings.HasPrefix(model, "minimax"), strings.HasPrefix(model, "deepseek-r1"):
		levels = []string{"auto"}
	}
	return append([]string(nil), levels...)
}

func ValidateReasoningEffort(modelID, effort string) error {
	effort = EffectiveReasoningEffort(effort)
	for _, available := range SupportedReasoningEfforts(modelID) {
		if effort == available {
			return nil
		}
	}
	return fmt.Errorf("reasoning effort %q is unavailable for model %q; available: %s", effort, modelID, strings.Join(SupportedReasoningEfforts(modelID), ", "))
}

func normalizedModelID(modelID string) string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if index := strings.LastIndex(modelID, "/"); index >= 0 {
		modelID = modelID[index+1:]
	}
	return modelID
}

func isOpenAIProModel(model string) bool {
	openAI := strings.HasPrefix(model, "gpt-5") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4")
	return openAI && (strings.HasSuffix(model, "-pro") || strings.Contains(model, "-pro-"))
}

func modernGPTXHigh(model string) bool {
	for _, prefix := range []string{"gpt-5.2", "gpt-5.3", "gpt-5.4", "gpt-5.5"} {
		if model == prefix || strings.HasPrefix(model, prefix+"-") || strings.HasPrefix(model, prefix+".") {
			return true
		}
	}
	return false
}

func modernClaudeOpus(model string) bool {
	const prefix = "claude-opus-"
	if !strings.HasPrefix(model, prefix) {
		return false
	}
	version := strings.FieldsFunc(strings.TrimPrefix(model, prefix), func(character rune) bool { return character == '.' || character == '-' })
	if len(version) == 0 {
		return false
	}
	major, err := strconv.Atoi(version[0])
	if err != nil || major < 4 {
		return false
	}
	if major > 4 {
		return true
	}
	if len(version) < 2 {
		return false
	}
	minor, err := strconv.Atoi(version[1])
	return err == nil && minor >= 7 && minor <= 99
}

type Config struct {
	Version               int                            `toml:"version" json:"version"`
	ActiveProvider        string                         `toml:"active_provider" json:"active_provider"`
	Providers             map[string]ProviderConfig      `toml:"providers" json:"providers"`
	MCPServers            map[string]MCPServerConfig     `toml:"mcp_servers,omitempty" json:"mcp_servers,omitempty"`
	SkillRegistries       map[string]SkillRegistryConfig `toml:"skill_registries,omitempty" json:"skill_registries,omitempty"`
	PermissionMode        string                         `toml:"permission_mode,omitempty" json:"permission_mode,omitempty"`
	RoutingMode           string                         `toml:"routing_mode,omitempty" json:"routing_mode,omitempty"`
	GradientEnabled       bool                           `toml:"gradient_enabled,omitempty" json:"gradient_enabled,omitempty"`
	MaxTurns              int                            `toml:"max_turns,omitempty" json:"max_turns,omitempty"`
	MaxTotalTokens        int                            `toml:"max_total_tokens,omitempty" json:"max_total_tokens,omitempty"`
	ToolTimeoutSec        int                            `toml:"tool_timeout_seconds,omitempty" json:"tool_timeout_seconds,omitempty"`
	MaxOutputBytes        int                            `toml:"max_output_bytes,omitempty" json:"max_output_bytes,omitempty"`
	MaxReadBytes          int                            `toml:"max_read_bytes,omitempty" json:"max_read_bytes,omitempty"`
	MaxSearchResults      int                            `toml:"max_search_results,omitempty" json:"max_search_results,omitempty"`
	MaxParallelTools      int                            `toml:"max_parallel_tools,omitempty" json:"max_parallel_tools,omitempty"`
	ReadOnlyCommands      []string                       `toml:"read_only_commands,omitempty" json:"read_only_commands,omitempty"`
	AutoAllowCommands     []string                       `toml:"auto_allow_commands,omitempty" json:"auto_allow_commands,omitempty"`
	DangerousCommands     []string                       `toml:"dangerous_commands,omitempty" json:"dangerous_commands,omitempty"`
	BlockedCommands       []string                       `toml:"blocked_commands,omitempty" json:"blocked_commands,omitempty"`
	ShellEnvironment      []string                       `toml:"shell_environment,omitempty" json:"shell_environment,omitempty"`
	TokenBytesPerToken    int                            `toml:"token_bytes_per_token,omitempty" json:"token_bytes_per_token,omitempty"`
	ReservedOutputTokens  int                            `toml:"reserved_output_tokens,omitempty" json:"reserved_output_tokens,omitempty"`
	ContextRecentRounds   int                            `toml:"context_recent_rounds,omitempty" json:"context_recent_rounds,omitempty"`
	ContextCompactTrigger int                            `toml:"context_compact_trigger_percent,omitempty" json:"context_compact_trigger_percent,omitempty"`
	ContextCompactTarget  int                            `toml:"context_compact_target_percent,omitempty" json:"context_compact_target_percent,omitempty"`
	MaxProjectMapBytes    int                            `toml:"max_project_map_bytes,omitempty" json:"max_project_map_bytes,omitempty"`
	MaxToolContextBytes   int                            `toml:"max_tool_context_bytes,omitempty" json:"max_tool_context_bytes,omitempty"`
	SkillCatalogPageBytes int                            `toml:"skill_catalog_page_bytes,omitempty" json:"skill_catalog_page_bytes,omitempty"`
	MaxSummaryBytes       int                            `toml:"max_summary_bytes,omitempty" json:"max_summary_bytes,omitempty"`
	MaxSessions           int                            `toml:"max_sessions,omitempty" json:"max_sessions,omitempty"`
	MaxSessionBytes       int64                          `toml:"max_session_bytes,omitempty" json:"max_session_bytes,omitempty"`
	ModelMetadata         ModelMetadataConfig            `toml:"model_metadata,omitempty" json:"model_metadata"`
}

func Default() Config {
	return Config{
		Version:               SchemaVersion,
		Providers:             make(map[string]ProviderConfig),
		MCPServers:            make(map[string]MCPServerConfig),
		SkillRegistries:       make(map[string]SkillRegistryConfig),
		PermissionMode:        "manual",
		RoutingMode:           "fixed",
		MaxTurns:              20,
		MaxTotalTokens:        1_000_000,
		ToolTimeoutSec:        60,
		MaxOutputBytes:        64 << 10,
		MaxReadBytes:          1 << 20,
		MaxSearchResults:      200,
		MaxParallelTools:      4,
		ReadOnlyCommands:      []string{"ls", "dir", "pwd", "find", "rg", "grep", "git status", "git diff", "git log", "git show", "git grep", "git branch", "git rev-parse", "git ls-files"},
		AutoAllowCommands:     []string{"ls", "dir", "pwd", "find", "rg", "grep", "git status", "git diff", "git log", "git show", "git grep", "git branch", "git rev-parse", "git ls-files", "go test", "go vet", "go build", "go list", "go env", "go version", "gofmt", "go fmt"},
		DangerousCommands:     []string{"rm -rf", "git reset --hard", "git clean -fd", "git push --force", "mkfs", "diskpart", "format ", "remove-item -recurse", "del /s", "rd /s"},
		TokenBytesPerToken:    4,
		ReservedOutputTokens:  8192,
		ContextRecentRounds:   3,
		ContextCompactTrigger: 85,
		ContextCompactTarget:  60,
		MaxProjectMapBytes:    32 << 10,
		MaxToolContextBytes:   8 << 10,
		SkillCatalogPageBytes: 8 << 10,
		MaxSummaryBytes:       16 << 10,
		MaxSessions:           100,
		MaxSessionBytes:       512 << 20,
		ModelMetadata: ModelMetadataConfig{
			Enabled: true, CatalogURL: "https://models.dev/api.json", RequestTimeoutSeconds: 5,
			EndpointTTLHours: 24, CatalogTTLHours: 24, StaleTTLHours: 7 * 24,
			NegativeTTLMinutes: 60, LearnedTTLHours: 30 * 24,
			MaxResponseBytes: 16 << 20, MaxCacheEntries: 1000,
			ProbeTiers: []int{256_000, 128_000, 64_000, 32_000, 16_000, 8_000},
		},
	}
}

func (c Config) Clone() Config {
	clone := c
	clone.ReadOnlyCommands = append([]string(nil), c.ReadOnlyCommands...)
	clone.AutoAllowCommands = append([]string(nil), c.AutoAllowCommands...)
	clone.DangerousCommands = append([]string(nil), c.DangerousCommands...)
	clone.BlockedCommands = append([]string(nil), c.BlockedCommands...)
	clone.ShellEnvironment = append([]string(nil), c.ShellEnvironment...)
	clone.ModelMetadata.ProbeTiers = append([]int(nil), c.ModelMetadata.ProbeTiers...)
	clone.Providers = make(map[string]ProviderConfig, len(c.Providers))
	for name, provider := range c.Providers {
		provider.Headers = cloneStringMap(provider.Headers)
		provider.Routing.Tasks = append([]string(nil), provider.Routing.Tasks...)
		clone.Providers[name] = provider
	}
	clone.MCPServers = make(map[string]MCPServerConfig, len(c.MCPServers))
	for name, server := range c.MCPServers {
		server.Args = append([]string(nil), server.Args...)
		server.Environment = append([]string(nil), server.Environment...)
		server.ReadOnlyTools = append([]string(nil), server.ReadOnlyTools...)
		clone.MCPServers[name] = server
	}
	clone.SkillRegistries = make(map[string]SkillRegistryConfig, len(c.SkillRegistries))
	for name, registry := range c.SkillRegistries {
		registry.PublicKeys = cloneStringMap(registry.PublicKeys)
		clone.SkillRegistries[name] = registry
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
	if len(c.MCPServers) > 32 {
		return errors.New("mcp server limit exceeds 32")
	}
	for name, server := range c.MCPServers {
		if err := validateMCPServer(name, server); err != nil {
			return err
		}
	}
	if len(c.SkillRegistries) > 16 {
		return errors.New("skill registry limit exceeds 16")
	}
	for name, registry := range c.SkillRegistries {
		if err := validateSkillRegistry(name, registry); err != nil {
			return err
		}
	}
	switch c.RoutingMode {
	case "fixed", "auto":
	default:
		return fmt.Errorf("invalid routing_mode %q", c.RoutingMode)
	}
	if c.MaxTurns <= 0 || c.MaxTotalTokens <= 0 {
		return errors.New("turn and token budgets must be greater than zero")
	}
	if c.ToolTimeoutSec <= 0 || c.MaxOutputBytes <= 0 || c.MaxReadBytes <= 0 || c.MaxSearchResults <= 0 || c.MaxParallelTools <= 0 {
		return errors.New("resource limits must be greater than zero")
	}
	if c.TokenBytesPerToken <= 0 || c.ReservedOutputTokens <= 0 || c.ContextRecentRounds <= 0 || c.MaxProjectMapBytes <= 0 || c.MaxToolContextBytes <= 0 || c.SkillCatalogPageBytes <= 0 || c.MaxSummaryBytes <= 0 {
		return errors.New("context limits must be greater than zero")
	}
	if c.ContextCompactTarget < 1 || c.ContextCompactTrigger > 99 || c.ContextCompactTarget >= c.ContextCompactTrigger {
		return errors.New("context compaction percentages must satisfy 1 <= target < trigger <= 99")
	}
	if c.MaxSessions <= 0 || c.MaxSessionBytes <= 0 {
		return errors.New("session limits must be greater than zero")
	}
	if err := validateModelMetadata(c.ModelMetadata); err != nil {
		return err
	}
	switch c.PermissionMode {
	case "manual", "plan", "auto", "full":
	default:
		return fmt.Errorf("invalid permission_mode %q", c.PermissionMode)
	}
	return nil
}

func validateModelMetadata(metadata ModelMetadataConfig) error {
	if strings.TrimSpace(metadata.CatalogURL) == "" {
		return errors.New("model metadata catalog_url is required")
	}
	parsed, err := url.Parse(metadata.CatalogURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("model metadata catalog_url must be an absolute HTTP(S) URL")
	}
	if metadata.RequestTimeoutSeconds <= 0 || metadata.EndpointTTLHours <= 0 || metadata.CatalogTTLHours <= 0 || metadata.StaleTTLHours <= 0 || metadata.NegativeTTLMinutes <= 0 || metadata.LearnedTTLHours <= 0 || metadata.MaxResponseBytes <= 0 || metadata.MaxCacheEntries <= 0 {
		return errors.New("model metadata limits must be greater than zero")
	}
	if len(metadata.ProbeTiers) == 0 {
		return errors.New("model metadata probe_tiers is required")
	}
	previous := int(^uint(0) >> 1)
	for _, tier := range metadata.ProbeTiers {
		if tier <= 0 || tier >= previous {
			return errors.New("model metadata probe_tiers must be positive and strictly descending")
		}
		previous = tier
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
	if err := ValidateReasoningEffort(p.Model, p.ReasoningEffort); err != nil {
		return fmt.Errorf("provider %q %w", name, err)
	}
	if p.ContextWindow < 0 || p.TimeoutSeconds < 0 {
		return fmt.Errorf("provider %q numeric limits cannot be negative", name)
	}
	if p.Routing.InputCostPerMillion < 0 || p.Routing.OutputCostPerMillion < 0 {
		return fmt.Errorf("provider %q routing costs cannot be negative", name)
	}
	validTasks := map[string]bool{"general": true, "coding": true, "review": true, "debugging": true, "testing": true, "documentation": true}
	seenTasks := make(map[string]bool, len(p.Routing.Tasks))
	for _, task := range p.Routing.Tasks {
		if !validTasks[task] {
			return fmt.Errorf("provider %q routing task %q is invalid", name, task)
		}
		if seenTasks[task] {
			return fmt.Errorf("provider %q routing task %q is duplicated", name, task)
		}
		seenTasks[task] = true
	}
	return nil
}

var (
	mcpNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func validateMCPServer(name string, server MCPServerConfig) error {
	if !mcpNamePattern.MatchString(name) {
		return fmt.Errorf("mcp server name %q is invalid", name)
	}
	if server.Disabled {
		return nil
	}
	if strings.TrimSpace(server.Command) == "" {
		return fmt.Errorf("mcp server %q command is required", name)
	}
	if server.TimeoutSeconds < 0 {
		return fmt.Errorf("mcp server %q timeout cannot be negative", name)
	}
	seenEnvironment := make(map[string]bool, len(server.Environment))
	for _, environment := range server.Environment {
		if !envNamePattern.MatchString(environment) {
			return fmt.Errorf("mcp server %q environment entry %q must be a variable name without a value", name, environment)
		}
		if seenEnvironment[environment] {
			return fmt.Errorf("mcp server %q environment entry %q is duplicated", name, environment)
		}
		seenEnvironment[environment] = true
	}
	seenTools := make(map[string]bool, len(server.ReadOnlyTools))
	for _, toolName := range server.ReadOnlyTools {
		if strings.TrimSpace(toolName) == "" || seenTools[toolName] {
			return fmt.Errorf("mcp server %q contains an invalid or duplicate read-only tool name", name)
		}
		seenTools[toolName] = true
	}
	return nil
}

func validateSkillRegistry(name string, registry SkillRegistryConfig) error {
	if !mcpNamePattern.MatchString(name) {
		return fmt.Errorf("skill registry name %q is invalid", name)
	}
	if registry.Disabled {
		return nil
	}
	parsed, err := url.Parse(registry.IndexURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopbackHost(parsed.Hostname()))) {
		return fmt.Errorf("skill registry %q index_url must use HTTPS or loopback HTTP", name)
	}
	if registry.TimeoutSeconds < 0 {
		return fmt.Errorf("skill registry %q timeout cannot be negative", name)
	}
	if registry.TokenEnvironment != "" && !envNamePattern.MatchString(registry.TokenEnvironment) {
		return fmt.Errorf("skill registry %q token_environment must be a variable name", name)
	}
	if len(registry.PublicKeys) == 0 {
		return fmt.Errorf("skill registry %q requires at least one Ed25519 public key", name)
	}
	for keyID, encoded := range registry.PublicKeys {
		if !mcpNamePattern.MatchString(keyID) {
			return fmt.Errorf("skill registry %q key ID %q is invalid", name, keyID)
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(decoded) != ed25519.PublicKeySize {
			return fmt.Errorf("skill registry %q key %q is not an Ed25519 public key", name, keyID)
		}
	}
	return nil
}

func loopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

type LoadOptions struct {
	ExplicitPath string
	Workspace    string
	Environ      []string
}

type Loaded struct {
	Config    Config
	Path      string
	Workspace string
	Store     *Store
}

func Load(opts LoadOptions) (Loaded, error) {
	store, err := openStore(opts)
	if err != nil {
		return Loaded{}, err
	}
	return Loaded{Config: store.Config(), Path: store.Path(), Workspace: store.Workspace(), Store: store}, nil
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
	return saveFileConfig(path, fileConfigFromResolved(cfg))
}

func saveConfigBytes(path string, data []byte) error {
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
	if value := env["EYLU_PERMISSION_MODE"]; value != "" {
		cfg.PermissionMode = value
	}
	if value := env["EYLU_ROUTING_MODE"]; value != "" {
		cfg.RoutingMode = value
	}
	if value := env["EYLU_MODEL_METADATA_ENABLED"]; value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			cfg.ModelMetadata.Enabled = parsed
		}
	}
	for key, target := range map[string]*int{
		"EYLU_MAX_TURNS":                       &cfg.MaxTurns,
		"EYLU_MAX_TOTAL_TOKENS":                &cfg.MaxTotalTokens,
		"EYLU_TOOL_TIMEOUT":                    &cfg.ToolTimeoutSec,
		"EYLU_MAX_OUTPUT_BYTES":                &cfg.MaxOutputBytes,
		"EYLU_MAX_PARALLEL_TOOLS":              &cfg.MaxParallelTools,
		"EYLU_TOKEN_BYTES_PER_TOKEN":           &cfg.TokenBytesPerToken,
		"EYLU_RESERVED_OUTPUT_TOKENS":          &cfg.ReservedOutputTokens,
		"EYLU_CONTEXT_RECENT_ROUNDS":           &cfg.ContextRecentRounds,
		"EYLU_CONTEXT_COMPACT_TRIGGER_PERCENT": &cfg.ContextCompactTrigger,
		"EYLU_CONTEXT_COMPACT_TARGET_PERCENT":  &cfg.ContextCompactTarget,
		"EYLU_MAX_PROJECT_MAP_BYTES":           &cfg.MaxProjectMapBytes,
		"EYLU_MAX_TOOL_CONTEXT_BYTES":          &cfg.MaxToolContextBytes,
		"EYLU_SKILL_CATALOG_PAGE_BYTES":        &cfg.SkillCatalogPageBytes,
		"EYLU_MAX_SUMMARY_BYTES":               &cfg.MaxSummaryBytes,
		"EYLU_MAX_SESSIONS":                    &cfg.MaxSessions,
	} {
		if value := env[key]; value != "" {
			if parsed, err := strconv.Atoi(value); err == nil {
				*target = parsed
			}
		}
	}
	if value := env["EYLU_MAX_SESSION_BYTES"]; value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
			cfg.MaxSessionBytes = parsed
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
