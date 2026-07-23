package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"

	"github.com/pelletier/go-toml/v2"
)

// FileConfig is the presence-aware representation persisted to TOML. Runtime
// code consumes Config, which is always fully resolved against Default().
type FileConfig struct {
	Version                *int                           `toml:"version,omitempty"`
	ActiveProvider         *string                        `toml:"active_provider,omitempty"`
	Providers              map[string]FileProviderConfig  `toml:"providers,omitempty"`
	RemovedProviders       []string                       `toml:"removed_providers,omitempty"`
	MCPServers             map[string]FileMCPServerConfig `toml:"mcp_servers,omitempty"`
	RemovedMCPServers      []string                       `toml:"removed_mcp_servers,omitempty"`
	SkillRegistries        map[string]FileSkillRegistry   `toml:"skill_registries,omitempty"`
	RemovedSkillRegistries []string                       `toml:"removed_skill_registries,omitempty"`
	PermissionMode         *string                        `toml:"permission_mode,omitempty"`
	RoutingMode            *string                        `toml:"routing_mode,omitempty"`
	GradientEnabled        *bool                          `toml:"gradient_enabled,omitempty"`
	MaxTurns               *int                           `toml:"max_turns,omitempty"`
	MaxTotalTokens         *int                           `toml:"max_total_tokens,omitempty"`
	ToolTimeoutSec         *int                           `toml:"tool_timeout_seconds,omitempty"`
	MaxOutputBytes         *int                           `toml:"max_output_bytes,omitempty"`
	MaxReadBytes           *int                           `toml:"max_read_bytes,omitempty"`
	MaxSearchResults       *int                           `toml:"max_search_results,omitempty"`
	MaxParallelTools       *int                           `toml:"max_parallel_tools,omitempty"`
	MaxParallelAgents      *int                           `toml:"max_parallel_agents,omitempty"`
	CodeContextCacheBytes  *int64                         `toml:"code_context_cache_bytes,omitempty"`
	MaxReadLines           *int                           `toml:"max_read_lines,omitempty"`
	CodeIndexWorkers       *int                           `toml:"code_index_workers,omitempty"`
	SearchAgent            *FileSearchAgentConfig         `toml:"search_agent,omitempty"`
	ReadOnlyCommands       *[]string                      `toml:"read_only_commands,omitempty"`
	AutoAllowCommands      *[]string                      `toml:"auto_allow_commands,omitempty"`
	DangerousCommands      *[]string                      `toml:"dangerous_commands,omitempty"`
	BlockedCommands        *[]string                      `toml:"blocked_commands,omitempty"`
	ShellEnvironment       *[]string                      `toml:"shell_environment,omitempty"`
	TokenBytesPerToken     *int                           `toml:"token_bytes_per_token,omitempty"`
	ReservedOutputTokens   *int                           `toml:"reserved_output_tokens,omitempty"`
	ContextRecentRounds    *int                           `toml:"context_recent_rounds,omitempty"`
	ContextCompactTrigger  *int                           `toml:"context_compact_trigger_percent,omitempty"`
	ContextCompactTarget   *int                           `toml:"context_compact_target_percent,omitempty"`
	MaxProjectMapBytes     *int                           `toml:"max_project_map_bytes,omitempty"`
	MaxToolContextBytes    *int                           `toml:"max_tool_context_bytes,omitempty"`
	SkillCatalogPageBytes  *int                           `toml:"skill_catalog_page_bytes,omitempty"`
	MaxSummaryBytes        *int                           `toml:"max_summary_bytes,omitempty"`
	MaxSessions            *int                           `toml:"max_sessions,omitempty"`
	MaxSessionBytes        *int64                         `toml:"max_session_bytes,omitempty"`
	ModelMetadata          *FileModelMetadataConfig       `toml:"model_metadata,omitempty"`
}

type FileProviderConfig struct {
	Adapter         *string                 `toml:"adapter,omitempty"`
	BaseURL         *string                 `toml:"base_url,omitempty"`
	APIKey          *string                 `toml:"api_key,omitempty"`
	Model           *string                 `toml:"model,omitempty"`
	ReasoningEffort *string                 `toml:"reasoning_effort,omitempty"`
	CatalogProvider *string                 `toml:"catalog_provider,omitempty"`
	ContextWindow   *int                    `toml:"context_window,omitempty"`
	TimeoutSeconds  *int                    `toml:"timeout_seconds,omitempty"`
	Headers         *map[string]string      `toml:"headers,omitempty"`
	Routing         *FileProviderRouting    `toml:"routing,omitempty"`
	WebTools        *WebToolsConfig         `toml:"web_tools,omitempty"`
	WebCapabilities *WebCapabilityOverrides `toml:"web_capabilities,omitempty"`
}

type FileProviderRouting struct {
	Tasks                *[]string `toml:"tasks,omitempty"`
	Priority             *int      `toml:"priority,omitempty"`
	InputCostPerMillion  *float64  `toml:"input_cost_per_million,omitempty"`
	OutputCostPerMillion *float64  `toml:"output_cost_per_million,omitempty"`
}

type FileMCPServerConfig struct {
	Transport              *string             `toml:"transport,omitempty"`
	Enabled                *bool               `toml:"enabled,omitempty"`
	Required               *bool               `toml:"required,omitempty"`
	StartupTimeoutSeconds  *int                `toml:"startup_timeout_seconds,omitempty"`
	CallTimeoutSeconds     *int                `toml:"call_timeout_seconds,omitempty"`
	AllowTools             *[]string           `toml:"allow_tools,omitempty"`
	DenyTools              *[]string           `toml:"deny_tools,omitempty"`
	Command                *string             `toml:"command,omitempty"`
	Args                   *[]string           `toml:"args,omitempty"`
	Environment            *[]string           `toml:"environment,omitempty"`
	WorkingDirectory       *string             `toml:"working_directory,omitempty"`
	URL                    *string             `toml:"url,omitempty"`
	Headers                *map[string]string  `toml:"headers,omitempty"`
	EnvironmentHeaders     *map[string]string  `toml:"environment_headers,omitempty"`
	BearerTokenEnvironment *string             `toml:"bearer_token_environment,omitempty"`
	OAuth                  *FileMCPOAuthConfig `toml:"oauth,omitempty"`
	ReadOnlyTools          *[]string           `toml:"read_only_tools,omitempty"`
	TimeoutSeconds         *int                `toml:"timeout_seconds,omitempty"`
	Disabled               *bool               `toml:"disabled,omitempty"`
}

type FileMCPOAuthConfig struct {
	Issuer                  *string   `toml:"issuer,omitempty"`
	ClientID                *string   `toml:"client_id,omitempty"`
	ClientSecretEnvironment *string   `toml:"client_secret_environment,omitempty"`
	Scopes                  *[]string `toml:"scopes,omitempty"`
	RedirectURL             *string   `toml:"redirect_url,omitempty"`
}

type FileSkillRegistry struct {
	IndexURL         *string            `toml:"index_url,omitempty"`
	PublicKeys       *map[string]string `toml:"public_keys,omitempty"`
	TokenEnvironment *string            `toml:"token_environment,omitempty"`
	TimeoutSeconds   *int               `toml:"timeout_seconds,omitempty"`
	Disabled         *bool              `toml:"disabled,omitempty"`
}

type FileModelMetadataConfig struct {
	Enabled               *bool   `toml:"enabled,omitempty"`
	CatalogURL            *string `toml:"catalog_url,omitempty"`
	RequestTimeoutSeconds *int    `toml:"request_timeout_seconds,omitempty"`
	EndpointTTLHours      *int    `toml:"endpoint_ttl_hours,omitempty"`
	CatalogTTLHours       *int    `toml:"catalog_ttl_hours,omitempty"`
	StaleTTLHours         *int    `toml:"stale_ttl_hours,omitempty"`
	NegativeTTLMinutes    *int    `toml:"negative_ttl_minutes,omitempty"`
	LearnedTTLHours       *int    `toml:"learned_ttl_hours,omitempty"`
	MaxResponseBytes      *int    `toml:"max_response_bytes,omitempty"`
	MaxCacheEntries       *int    `toml:"max_cache_entries,omitempty"`
	ProbeTiers            *[]int  `toml:"probe_tiers,omitempty"`
}

type FileSearchAgentConfig struct {
	Provider       *string `toml:"provider,omitempty"`
	Model          *string `toml:"model,omitempty"`
	MaxTurns       *int    `toml:"max_turns,omitempty"`
	TimeoutSeconds *int    `toml:"timeout_seconds,omitempty"`
}

type ValuePatch[T any] struct {
	Value  T
	Set    bool
	Remove bool
}

func SetValue[T any](value T) ValuePatch[T] { return ValuePatch[T]{Value: value, Set: true} }
func RemoveValue[T any]() ValuePatch[T]     { return ValuePatch[T]{Remove: true} }

type ProviderPatch struct {
	Adapter         ValuePatch[string]
	BaseURL         ValuePatch[string]
	APIKey          ValuePatch[string]
	Model           ValuePatch[string]
	ReasoningEffort ValuePatch[string]
	CatalogProvider ValuePatch[string]
	ContextWindow   ValuePatch[int]
	TimeoutSeconds  ValuePatch[int]
	Headers         ValuePatch[map[string]string]
	RoutingTasks    ValuePatch[[]string]
	RoutingPriority ValuePatch[int]
	InputCost       ValuePatch[float64]
	OutputCost      ValuePatch[float64]
	WebTools        ValuePatch[WebToolsConfig]
	WebCapabilities ValuePatch[WebCapabilityOverrides]
}

func (patch ProviderPatch) Empty() bool { return reflect.ValueOf(patch).IsZero() }

func SparseProviderPatch(provider ProviderConfig) ProviderPatch {
	patch := ProviderPatch{
		Adapter: SetValue(provider.Adapter), BaseURL: SetValue(provider.BaseURL),
		Model: SetValue(provider.Model),
	}
	if provider.APIKey != "" {
		patch.APIKey = SetValue(provider.APIKey)
	}
	if provider.ReasoningEffort != "" {
		patch.ReasoningEffort = SetValue(provider.ReasoningEffort)
	}
	if provider.CatalogProvider != "" {
		patch.CatalogProvider = SetValue(provider.CatalogProvider)
	}
	if provider.ContextWindow != 0 {
		patch.ContextWindow = SetValue(provider.ContextWindow)
	}
	if provider.TimeoutSeconds != 0 {
		patch.TimeoutSeconds = SetValue(provider.TimeoutSeconds)
	}
	if provider.Headers != nil {
		patch.Headers = SetValue(cloneStringMap(provider.Headers))
	}
	if provider.Routing.Tasks != nil {
		patch.RoutingTasks = SetValue(append([]string(nil), provider.Routing.Tasks...))
	}
	if provider.Routing.Priority != 0 {
		patch.RoutingPriority = SetValue(provider.Routing.Priority)
	}
	if provider.Routing.InputCostPerMillion != 0 {
		patch.InputCost = SetValue(provider.Routing.InputCostPerMillion)
	}
	if provider.Routing.OutputCostPerMillion != 0 {
		patch.OutputCost = SetValue(provider.Routing.OutputCostPerMillion)
	}
	if !reflect.ValueOf(provider.WebTools).IsZero() {
		patch.WebTools = SetValue(cloneWebTools(provider.WebTools))
	}
	if !reflect.ValueOf(provider.WebCapabilities).IsZero() {
		patch.WebCapabilities = SetValue(provider.WebCapabilities)
	}
	return patch
}

func CompleteProviderPatch(provider ProviderConfig) ProviderPatch {
	return ProviderPatch{
		Adapter: SetValue(provider.Adapter), BaseURL: SetValue(provider.BaseURL), APIKey: SetValue(provider.APIKey), Model: SetValue(provider.Model), ReasoningEffort: SetValue(provider.ReasoningEffort),
		CatalogProvider: SetValue(provider.CatalogProvider), ContextWindow: SetValue(provider.ContextWindow), TimeoutSeconds: SetValue(provider.TimeoutSeconds),
		Headers: SetValue(cloneStringMap(provider.Headers)), RoutingTasks: SetValue(append([]string(nil), provider.Routing.Tasks...)),
		RoutingPriority: SetValue(provider.Routing.Priority), InputCost: SetValue(provider.Routing.InputCostPerMillion), OutputCost: SetValue(provider.Routing.OutputCostPerMillion),
		WebTools: SetValue(cloneWebTools(provider.WebTools)), WebCapabilities: SetValue(provider.WebCapabilities),
	}
}

func ApplyProviderPatch(provider ProviderConfig, patch ProviderPatch) ProviderConfig {
	applyValue := func(target *string, value ValuePatch[string]) {
		if value.Remove {
			*target = ""
		} else if value.Set {
			*target = value.Value
		}
	}
	applyInt := func(target *int, value ValuePatch[int]) {
		if value.Remove {
			*target = 0
		} else if value.Set {
			*target = value.Value
		}
	}
	applyFloat := func(target *float64, value ValuePatch[float64]) {
		if value.Remove {
			*target = 0
		} else if value.Set {
			*target = value.Value
		}
	}
	applyValue(&provider.Adapter, patch.Adapter)
	applyValue(&provider.BaseURL, patch.BaseURL)
	applyValue(&provider.APIKey, patch.APIKey)
	applyValue(&provider.Model, patch.Model)
	applyValue(&provider.ReasoningEffort, patch.ReasoningEffort)
	applyValue(&provider.CatalogProvider, patch.CatalogProvider)
	applyInt(&provider.ContextWindow, patch.ContextWindow)
	applyInt(&provider.TimeoutSeconds, patch.TimeoutSeconds)
	if patch.Headers.Remove {
		provider.Headers = nil
	} else if patch.Headers.Set {
		provider.Headers = cloneStringMap(patch.Headers.Value)
	}
	if patch.RoutingTasks.Remove {
		provider.Routing.Tasks = nil
	} else if patch.RoutingTasks.Set {
		provider.Routing.Tasks = append([]string(nil), patch.RoutingTasks.Value...)
	}
	applyInt(&provider.Routing.Priority, patch.RoutingPriority)
	applyFloat(&provider.Routing.InputCostPerMillion, patch.InputCost)
	applyFloat(&provider.Routing.OutputCostPerMillion, patch.OutputCost)
	if patch.WebTools.Remove {
		provider.WebTools = WebToolsConfig{}
	} else if patch.WebTools.Set {
		provider.WebTools = cloneWebTools(patch.WebTools.Value)
	}
	if patch.WebCapabilities.Remove {
		provider.WebCapabilities = WebCapabilityOverrides{}
	} else if patch.WebCapabilities.Set {
		provider.WebCapabilities = patch.WebCapabilities.Value
	}
	return provider
}

type Store struct {
	mu        sync.Mutex
	path      string
	workspace string
	environ   []string
	paths     []string
	files     map[string]FileConfig
	config    Config
}

func openStore(opts LoadOptions) (*Store, error) {
	workspace, err := filepath.Abs(opts.Workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	paths := configPaths(opts.ExplicitPath, workspace)
	files := make(map[string]FileConfig, len(paths))
	writePath := opts.ExplicitPath
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("read config %s: %w", path, readErr)
		}
		var file FileConfig
		if err := toml.Unmarshal(data, &file); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
		files[path] = file
		writePath = path
	}
	if writePath == "" {
		writePath = defaultUserConfigPath()
	}
	if _, ok := files[writePath]; !ok {
		files[writePath] = FileConfig{Version: ptr(SchemaVersion)}
	}
	store := &Store{path: writePath, workspace: workspace, environ: append([]string(nil), opts.Environ...), paths: paths, files: files}
	resolved, err := store.resolve(files)
	if err != nil {
		return nil, err
	}
	store.config = resolved
	return store, nil
}

func (s *Store) Path() string      { return s.path }
func (s *Store) Workspace() string { return s.workspace }

func (s *Store) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config.Clone()
}

func (s *Store) UpdateProvider(name string, patch ProviderPatch, activate bool) (Config, error) {
	return s.update(func(file *FileConfig) {
		if !patch.Empty() {
			if file.Providers == nil {
				file.Providers = make(map[string]FileProviderConfig)
			}
			provider := file.Providers[name]
			applyPatch(&provider.Adapter, patch.Adapter)
			applyPatch(&provider.BaseURL, patch.BaseURL)
			applyPatch(&provider.APIKey, patch.APIKey)
			applyPatch(&provider.Model, patch.Model)
			applyPatch(&provider.ReasoningEffort, patch.ReasoningEffort)
			applyPatch(&provider.CatalogProvider, patch.CatalogProvider)
			applyPatch(&provider.ContextWindow, patch.ContextWindow)
			applyPatch(&provider.TimeoutSeconds, patch.TimeoutSeconds)
			applyPatch(&provider.Headers, patch.Headers)
			applyPatch(&provider.WebTools, patch.WebTools)
			applyPatch(&provider.WebCapabilities, patch.WebCapabilities)
			if patch.RoutingTasks.Set || patch.RoutingTasks.Remove || patch.RoutingPriority.Set || patch.RoutingPriority.Remove || patch.InputCost.Set || patch.InputCost.Remove || patch.OutputCost.Set || patch.OutputCost.Remove {
				if provider.Routing == nil {
					provider.Routing = &FileProviderRouting{}
				}
				applyPatch(&provider.Routing.Tasks, patch.RoutingTasks)
				applyPatch(&provider.Routing.Priority, patch.RoutingPriority)
				applyPatch(&provider.Routing.InputCostPerMillion, patch.InputCost)
				applyPatch(&provider.Routing.OutputCostPerMillion, patch.OutputCost)
				if provider.Routing.Tasks == nil && provider.Routing.Priority == nil && provider.Routing.InputCostPerMillion == nil && provider.Routing.OutputCostPerMillion == nil {
					provider.Routing = nil
				}
			}
			file.Providers[name] = provider
			file.RemovedProviders = removeName(file.RemovedProviders, name)
		}
		if activate {
			file.ActiveProvider = ptr(name)
		}
	})
}

func (s *Store) SetActiveProvider(name string) (Config, error) {
	return s.update(func(file *FileConfig) { file.ActiveProvider = ptr(name) })
}

func (s *Store) SetGradientEnabled(value bool) (Config, error) {
	return s.update(func(file *FileConfig) { file.GradientEnabled = ptr(value) })
}

func (s *Store) DeleteProvider(name, active string) (Config, error) {
	return s.update(func(file *FileConfig) {
		delete(file.Providers, name)
		file.RemovedProviders = addName(file.RemovedProviders, name)
		file.ActiveProvider = ptr(active)
	})
}

func (s *Store) SetSkillRegistry(name string, registry SkillRegistryConfig) (Config, error) {
	return s.update(func(file *FileConfig) {
		if file.SkillRegistries == nil {
			file.SkillRegistries = make(map[string]FileSkillRegistry)
		}
		file.SkillRegistries[name] = fileSkillRegistry(registry)
		file.RemovedSkillRegistries = removeName(file.RemovedSkillRegistries, name)
	})
}

func (s *Store) DeleteSkillRegistry(name string) (Config, error) {
	return s.update(func(file *FileConfig) {
		delete(file.SkillRegistries, name)
		file.RemovedSkillRegistries = addName(file.RemovedSkillRegistries, name)
	})
}

func (s *Store) MCPServerSource(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mcpServerSourceLocked(name)
}

func (s *Store) SetMCPServerEnabled(name string, enabled bool) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, ok := s.mcpServerSourceLocked(name)
	if !ok {
		return Config{}, fmt.Errorf("mcp server %q does not exist", name)
	}
	return s.updateLocked(path, func(file *FileConfig) {
		server := file.MCPServers[name]
		server.Enabled = ptr(enabled)
		server.Disabled = nil
		file.MCPServers[name] = server
	})
}

func (s *Store) mcpServerSourceLocked(name string) (string, bool) {
	for index := len(s.paths) - 1; index >= 0; index-- {
		path := s.paths[index]
		if _, ok := s.files[path].MCPServers[name]; ok {
			return path, true
		}
	}
	return "", false
}

func (s *Store) update(mutator func(*FileConfig)) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateLocked(s.path, mutator)
}

func (s *Store) updateLocked(path string, mutator func(*FileConfig)) (Config, error) {
	file, err := cloneFileConfig(s.files[path])
	if err != nil {
		return Config{}, err
	}
	mutator(&file)
	files := make(map[string]FileConfig, len(s.files))
	for path, existing := range s.files {
		files[path] = existing
	}
	files[path] = file
	resolved, err := s.resolve(files)
	if err != nil {
		return Config{}, err
	}
	if err := saveFileConfig(path, file); err != nil {
		return Config{}, err
	}
	s.files = files
	s.config = resolved
	return resolved.Clone(), nil
}

func (s *Store) resolve(files map[string]FileConfig) (Config, error) {
	result := Default()
	for _, path := range s.paths {
		if file, ok := files[path]; ok {
			if err := validateFileConfig(file); err != nil {
				return Config{}, fmt.Errorf("config %s: %w", path, err)
			}
			applyFileConfig(&result, file)
		}
	}
	applyEnvironment(&result, s.environ)
	if err := result.Validate(); err != nil {
		return Config{}, err
	}
	return result, nil
}

func validateFileConfig(file FileConfig) error {
	for name, server := range file.MCPServers {
		if server.Enabled != nil && server.Disabled != nil {
			return fmt.Errorf("mcp server %q enabled and disabled fields conflict", name)
		}
		if server.TimeoutSeconds != nil && (server.StartupTimeoutSeconds != nil || server.CallTimeoutSeconds != nil) {
			return fmt.Errorf("mcp server %q timeout_seconds conflicts with startup_timeout_seconds or call_timeout_seconds", name)
		}
	}
	return nil
}

func saveFileConfig(path string, file FileConfig) error {
	data, err := toml.Marshal(file)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return saveConfigBytes(path, data)
}

func cloneFileConfig(file FileConfig) (FileConfig, error) {
	data, err := toml.Marshal(file)
	if err != nil {
		return FileConfig{}, err
	}
	var clone FileConfig
	if err := toml.Unmarshal(data, &clone); err != nil {
		return FileConfig{}, err
	}
	return clone, nil
}

func applyFileConfig(cfg *Config, file FileConfig) {
	assign(cfg.Version, file.Version, func(value int) { cfg.Version = value })
	assign(cfg.ActiveProvider, file.ActiveProvider, func(value string) { cfg.ActiveProvider = value })
	for name, overlay := range file.Providers {
		provider := cfg.Providers[name]
		applyFileProvider(&provider, overlay)
		cfg.Providers[name] = provider
	}
	for _, name := range file.RemovedProviders {
		delete(cfg.Providers, name)
	}
	for name, overlay := range file.MCPServers {
		server := cfg.MCPServers[name]
		applyFileMCPServer(&server, overlay)
		cfg.MCPServers[name] = server
	}
	for _, name := range file.RemovedMCPServers {
		delete(cfg.MCPServers, name)
	}
	for name, overlay := range file.SkillRegistries {
		registry := cfg.SkillRegistries[name]
		applyFileSkillRegistry(&registry, overlay)
		cfg.SkillRegistries[name] = registry
	}
	for _, name := range file.RemovedSkillRegistries {
		delete(cfg.SkillRegistries, name)
	}
	assign(cfg.PermissionMode, file.PermissionMode, func(value string) { cfg.PermissionMode = value })
	assign(cfg.RoutingMode, file.RoutingMode, func(value string) { cfg.RoutingMode = value })
	assign(cfg.GradientEnabled, file.GradientEnabled, func(value bool) { cfg.GradientEnabled = value })
	assign(cfg.MaxTurns, file.MaxTurns, func(value int) { cfg.MaxTurns = value })
	assign(cfg.MaxTotalTokens, file.MaxTotalTokens, func(value int) { cfg.MaxTotalTokens = value })
	assign(cfg.ToolTimeoutSec, file.ToolTimeoutSec, func(value int) { cfg.ToolTimeoutSec = value })
	assign(cfg.MaxOutputBytes, file.MaxOutputBytes, func(value int) { cfg.MaxOutputBytes = value })
	assign(cfg.MaxReadBytes, file.MaxReadBytes, func(value int) { cfg.MaxReadBytes = value })
	assign(cfg.MaxSearchResults, file.MaxSearchResults, func(value int) { cfg.MaxSearchResults = value })
	assign(cfg.MaxParallelTools, file.MaxParallelTools, func(value int) { cfg.MaxParallelTools = value })
	assign(cfg.MaxParallelAgents, file.MaxParallelAgents, func(value int) { cfg.MaxParallelAgents = value })
	assign(cfg.CodeContextCacheBytes, file.CodeContextCacheBytes, func(value int64) { cfg.CodeContextCacheBytes = value })
	assign(cfg.MaxReadLines, file.MaxReadLines, func(value int) { cfg.MaxReadLines = value })
	assign(cfg.CodeIndexWorkers, file.CodeIndexWorkers, func(value int) { cfg.CodeIndexWorkers = value })
	if file.SearchAgent != nil {
		assign(cfg.SearchAgent.Provider, file.SearchAgent.Provider, func(value string) { cfg.SearchAgent.Provider = value })
		assign(cfg.SearchAgent.Model, file.SearchAgent.Model, func(value string) { cfg.SearchAgent.Model = value })
		assign(cfg.SearchAgent.MaxTurns, file.SearchAgent.MaxTurns, func(value int) { cfg.SearchAgent.MaxTurns = value })
		assign(cfg.SearchAgent.TimeoutSeconds, file.SearchAgent.TimeoutSeconds, func(value int) { cfg.SearchAgent.TimeoutSeconds = value })
	}
	assignSlice(file.ReadOnlyCommands, func(value []string) { cfg.ReadOnlyCommands = value })
	assignSlice(file.AutoAllowCommands, func(value []string) { cfg.AutoAllowCommands = value })
	assignSlice(file.DangerousCommands, func(value []string) { cfg.DangerousCommands = value })
	assignSlice(file.BlockedCommands, func(value []string) { cfg.BlockedCommands = value })
	assignSlice(file.ShellEnvironment, func(value []string) { cfg.ShellEnvironment = value })
	assign(cfg.TokenBytesPerToken, file.TokenBytesPerToken, func(value int) { cfg.TokenBytesPerToken = value })
	assign(cfg.ReservedOutputTokens, file.ReservedOutputTokens, func(value int) { cfg.ReservedOutputTokens = value })
	assign(cfg.ContextRecentRounds, file.ContextRecentRounds, func(value int) { cfg.ContextRecentRounds = value })
	assign(cfg.ContextCompactTrigger, file.ContextCompactTrigger, func(value int) { cfg.ContextCompactTrigger = value })
	assign(cfg.ContextCompactTarget, file.ContextCompactTarget, func(value int) { cfg.ContextCompactTarget = value })
	assign(cfg.MaxProjectMapBytes, file.MaxProjectMapBytes, func(value int) { cfg.MaxProjectMapBytes = value })
	assign(cfg.MaxToolContextBytes, file.MaxToolContextBytes, func(value int) { cfg.MaxToolContextBytes = value })
	assign(cfg.SkillCatalogPageBytes, file.SkillCatalogPageBytes, func(value int) { cfg.SkillCatalogPageBytes = value })
	assign(cfg.MaxSummaryBytes, file.MaxSummaryBytes, func(value int) { cfg.MaxSummaryBytes = value })
	assign(cfg.MaxSessions, file.MaxSessions, func(value int) { cfg.MaxSessions = value })
	assign(cfg.MaxSessionBytes, file.MaxSessionBytes, func(value int64) { cfg.MaxSessionBytes = value })
	if file.ModelMetadata != nil {
		applyFileModelMetadata(&cfg.ModelMetadata, *file.ModelMetadata)
	}
}

func applyFileProvider(provider *ProviderConfig, file FileProviderConfig) {
	assign(provider.Adapter, file.Adapter, func(value string) { provider.Adapter = value })
	assign(provider.BaseURL, file.BaseURL, func(value string) { provider.BaseURL = value })
	assign(provider.APIKey, file.APIKey, func(value string) { provider.APIKey = value })
	assign(provider.Model, file.Model, func(value string) { provider.Model = value })
	assign(provider.ReasoningEffort, file.ReasoningEffort, func(value string) { provider.ReasoningEffort = value })
	assign(provider.CatalogProvider, file.CatalogProvider, func(value string) { provider.CatalogProvider = value })
	assign(provider.ContextWindow, file.ContextWindow, func(value int) { provider.ContextWindow = value })
	assign(provider.TimeoutSeconds, file.TimeoutSeconds, func(value int) { provider.TimeoutSeconds = value })
	if file.Headers != nil {
		provider.Headers = cloneStringMap(*file.Headers)
	}
	if file.Routing != nil {
		assignSlice(file.Routing.Tasks, func(value []string) { provider.Routing.Tasks = value })
		assign(provider.Routing.Priority, file.Routing.Priority, func(value int) { provider.Routing.Priority = value })
		assign(provider.Routing.InputCostPerMillion, file.Routing.InputCostPerMillion, func(value float64) { provider.Routing.InputCostPerMillion = value })
		assign(provider.Routing.OutputCostPerMillion, file.Routing.OutputCostPerMillion, func(value float64) { provider.Routing.OutputCostPerMillion = value })
	}
	if file.WebTools != nil {
		provider.WebTools = cloneWebTools(*file.WebTools)
	}
	if file.WebCapabilities != nil {
		provider.WebCapabilities = *file.WebCapabilities
	}
}

func applyFileMCPServer(server *MCPServerConfig, file FileMCPServerConfig) {
	assign(server.Transport, file.Transport, func(value string) { server.Transport = value })
	if file.Enabled != nil {
		server.Enabled = *file.Enabled
		server.Disabled = !*file.Enabled
	}
	if file.Disabled != nil {
		server.Disabled = *file.Disabled
		server.Enabled = !*file.Disabled
	}
	assign(server.Required, file.Required, func(value bool) { server.Required = value })
	if file.StartupTimeoutSeconds != nil {
		server.StartupTimeoutSeconds = *file.StartupTimeoutSeconds
		server.TimeoutSeconds = *file.StartupTimeoutSeconds
	}
	if file.TimeoutSeconds != nil {
		server.TimeoutSeconds = *file.TimeoutSeconds
		server.StartupTimeoutSeconds = *file.TimeoutSeconds
	}
	assign(server.CallTimeoutSeconds, file.CallTimeoutSeconds, func(value int) { server.CallTimeoutSeconds = value })
	assignSlice(file.AllowTools, func(value []string) { server.AllowTools = value })
	assignSlice(file.DenyTools, func(value []string) { server.DenyTools = value })
	assign(server.Command, file.Command, func(value string) { server.Command = value })
	assignSlice(file.Args, func(value []string) { server.Args = value })
	assignSlice(file.Environment, func(value []string) { server.Environment = value })
	assign(server.WorkingDirectory, file.WorkingDirectory, func(value string) { server.WorkingDirectory = value })
	assign(server.URL, file.URL, func(value string) { server.URL = value })
	if file.Headers != nil {
		server.Headers = cloneStringMap(*file.Headers)
	}
	if file.EnvironmentHeaders != nil {
		server.EnvironmentHeaders = cloneStringMap(*file.EnvironmentHeaders)
	}
	assign(server.BearerTokenEnvironment, file.BearerTokenEnvironment, func(value string) { server.BearerTokenEnvironment = value })
	if file.OAuth != nil {
		if server.OAuth == nil {
			server.OAuth = &MCPOAuthConfig{}
		}
		assign(server.OAuth.Issuer, file.OAuth.Issuer, func(value string) { server.OAuth.Issuer = value })
		assign(server.OAuth.ClientID, file.OAuth.ClientID, func(value string) { server.OAuth.ClientID = value })
		assign(server.OAuth.ClientSecretEnvironment, file.OAuth.ClientSecretEnvironment, func(value string) { server.OAuth.ClientSecretEnvironment = value })
		assignSlice(file.OAuth.Scopes, func(value []string) { server.OAuth.Scopes = value })
		assign(server.OAuth.RedirectURL, file.OAuth.RedirectURL, func(value string) { server.OAuth.RedirectURL = value })
	}
	assignSlice(file.ReadOnlyTools, func(value []string) { server.ReadOnlyTools = value })
	server.Transport = server.EffectiveTransport()
	server.Enabled = !server.Disabled
}

func applyFileSkillRegistry(registry *SkillRegistryConfig, file FileSkillRegistry) {
	assign(registry.IndexURL, file.IndexURL, func(value string) { registry.IndexURL = value })
	if file.PublicKeys != nil {
		registry.PublicKeys = cloneStringMap(*file.PublicKeys)
	}
	assign(registry.TokenEnvironment, file.TokenEnvironment, func(value string) { registry.TokenEnvironment = value })
	assign(registry.TimeoutSeconds, file.TimeoutSeconds, func(value int) { registry.TimeoutSeconds = value })
	assign(registry.Disabled, file.Disabled, func(value bool) { registry.Disabled = value })
}

func applyFileModelMetadata(metadata *ModelMetadataConfig, file FileModelMetadataConfig) {
	assign(metadata.Enabled, file.Enabled, func(value bool) { metadata.Enabled = value })
	assign(metadata.CatalogURL, file.CatalogURL, func(value string) { metadata.CatalogURL = value })
	assign(metadata.RequestTimeoutSeconds, file.RequestTimeoutSeconds, func(value int) { metadata.RequestTimeoutSeconds = value })
	assign(metadata.EndpointTTLHours, file.EndpointTTLHours, func(value int) { metadata.EndpointTTLHours = value })
	assign(metadata.CatalogTTLHours, file.CatalogTTLHours, func(value int) { metadata.CatalogTTLHours = value })
	assign(metadata.StaleTTLHours, file.StaleTTLHours, func(value int) { metadata.StaleTTLHours = value })
	assign(metadata.NegativeTTLMinutes, file.NegativeTTLMinutes, func(value int) { metadata.NegativeTTLMinutes = value })
	assign(metadata.LearnedTTLHours, file.LearnedTTLHours, func(value int) { metadata.LearnedTTLHours = value })
	assign(metadata.MaxResponseBytes, file.MaxResponseBytes, func(value int) { metadata.MaxResponseBytes = value })
	assign(metadata.MaxCacheEntries, file.MaxCacheEntries, func(value int) { metadata.MaxCacheEntries = value })
	if file.ProbeTiers != nil {
		metadata.ProbeTiers = append([]int(nil), (*file.ProbeTiers)...)
	}
}

func fileConfigFromResolved(cfg Config) FileConfig {
	defaults := Default()
	file := FileConfig{Version: ptr(SchemaVersion)}
	if cfg.ActiveProvider != "" {
		file.ActiveProvider = ptr(cfg.ActiveProvider)
	}
	if len(cfg.Providers) > 0 {
		file.Providers = make(map[string]FileProviderConfig, len(cfg.Providers))
		for name, provider := range cfg.Providers {
			file.Providers[name] = fileProvider(provider)
		}
	}
	if len(cfg.MCPServers) > 0 {
		file.MCPServers = make(map[string]FileMCPServerConfig, len(cfg.MCPServers))
		for name, server := range cfg.MCPServers {
			file.MCPServers[name] = fileMCPServer(server)
		}
	}
	if len(cfg.SkillRegistries) > 0 {
		file.SkillRegistries = make(map[string]FileSkillRegistry, len(cfg.SkillRegistries))
		for name, registry := range cfg.SkillRegistries {
			file.SkillRegistries[name] = fileSkillRegistry(registry)
		}
	}
	setDifferent(&file.PermissionMode, cfg.PermissionMode, defaults.PermissionMode)
	setDifferent(&file.RoutingMode, cfg.RoutingMode, defaults.RoutingMode)
	setDifferent(&file.GradientEnabled, cfg.GradientEnabled, defaults.GradientEnabled)
	setDifferent(&file.MaxTurns, cfg.MaxTurns, defaults.MaxTurns)
	setDifferent(&file.MaxTotalTokens, cfg.MaxTotalTokens, defaults.MaxTotalTokens)
	setDifferent(&file.ToolTimeoutSec, cfg.ToolTimeoutSec, defaults.ToolTimeoutSec)
	setDifferent(&file.MaxOutputBytes, cfg.MaxOutputBytes, defaults.MaxOutputBytes)
	setDifferent(&file.MaxReadBytes, cfg.MaxReadBytes, defaults.MaxReadBytes)
	setDifferent(&file.MaxSearchResults, cfg.MaxSearchResults, defaults.MaxSearchResults)
	setDifferent(&file.MaxParallelTools, cfg.MaxParallelTools, defaults.MaxParallelTools)
	setDifferent(&file.MaxParallelAgents, cfg.MaxParallelAgents, defaults.MaxParallelAgents)
	setDifferent(&file.CodeContextCacheBytes, cfg.CodeContextCacheBytes, defaults.CodeContextCacheBytes)
	setDifferent(&file.MaxReadLines, cfg.MaxReadLines, defaults.MaxReadLines)
	setDifferent(&file.CodeIndexWorkers, cfg.CodeIndexWorkers, defaults.CodeIndexWorkers)
	searchAgent := FileSearchAgentConfig{}
	setDifferent(&searchAgent.Provider, cfg.SearchAgent.Provider, defaults.SearchAgent.Provider)
	setDifferent(&searchAgent.Model, cfg.SearchAgent.Model, defaults.SearchAgent.Model)
	setDifferent(&searchAgent.MaxTurns, cfg.SearchAgent.MaxTurns, defaults.SearchAgent.MaxTurns)
	setDifferent(&searchAgent.TimeoutSeconds, cfg.SearchAgent.TimeoutSeconds, defaults.SearchAgent.TimeoutSeconds)
	if !reflect.ValueOf(searchAgent).IsZero() {
		file.SearchAgent = &searchAgent
	}
	setSliceDifferent(&file.ReadOnlyCommands, cfg.ReadOnlyCommands, defaults.ReadOnlyCommands)
	setSliceDifferent(&file.AutoAllowCommands, cfg.AutoAllowCommands, defaults.AutoAllowCommands)
	setSliceDifferent(&file.DangerousCommands, cfg.DangerousCommands, defaults.DangerousCommands)
	setSliceDifferent(&file.BlockedCommands, cfg.BlockedCommands, defaults.BlockedCommands)
	setSliceDifferent(&file.ShellEnvironment, cfg.ShellEnvironment, defaults.ShellEnvironment)
	setDifferent(&file.TokenBytesPerToken, cfg.TokenBytesPerToken, defaults.TokenBytesPerToken)
	setDifferent(&file.ReservedOutputTokens, cfg.ReservedOutputTokens, defaults.ReservedOutputTokens)
	setDifferent(&file.ContextRecentRounds, cfg.ContextRecentRounds, defaults.ContextRecentRounds)
	setDifferent(&file.ContextCompactTrigger, cfg.ContextCompactTrigger, defaults.ContextCompactTrigger)
	setDifferent(&file.ContextCompactTarget, cfg.ContextCompactTarget, defaults.ContextCompactTarget)
	setDifferent(&file.MaxProjectMapBytes, cfg.MaxProjectMapBytes, defaults.MaxProjectMapBytes)
	setDifferent(&file.MaxToolContextBytes, cfg.MaxToolContextBytes, defaults.MaxToolContextBytes)
	setDifferent(&file.SkillCatalogPageBytes, cfg.SkillCatalogPageBytes, defaults.SkillCatalogPageBytes)
	setDifferent(&file.MaxSummaryBytes, cfg.MaxSummaryBytes, defaults.MaxSummaryBytes)
	setDifferent(&file.MaxSessions, cfg.MaxSessions, defaults.MaxSessions)
	setDifferent(&file.MaxSessionBytes, cfg.MaxSessionBytes, defaults.MaxSessionBytes)
	file.ModelMetadata = fileModelMetadata(cfg.ModelMetadata, defaults.ModelMetadata)
	return file
}

func fileProvider(provider ProviderConfig) FileProviderConfig {
	file := FileProviderConfig{Adapter: ptr(provider.Adapter), BaseURL: ptr(provider.BaseURL), Model: ptr(provider.Model)}
	if provider.APIKey != "" {
		file.APIKey = ptr(provider.APIKey)
	}
	if provider.ReasoningEffort != "" {
		file.ReasoningEffort = ptr(provider.ReasoningEffort)
	}
	if provider.CatalogProvider != "" {
		file.CatalogProvider = ptr(provider.CatalogProvider)
	}
	if provider.ContextWindow != 0 {
		file.ContextWindow = ptr(provider.ContextWindow)
	}
	if provider.TimeoutSeconds != 0 {
		file.TimeoutSeconds = ptr(provider.TimeoutSeconds)
	}
	if provider.Headers != nil {
		value := cloneStringMap(provider.Headers)
		file.Headers = &value
	}
	if provider.Routing.Tasks != nil || provider.Routing.Priority != 0 || provider.Routing.InputCostPerMillion != 0 || provider.Routing.OutputCostPerMillion != 0 {
		file.Routing = &FileProviderRouting{}
		if provider.Routing.Tasks != nil {
			value := append([]string(nil), provider.Routing.Tasks...)
			file.Routing.Tasks = &value
		}
		if provider.Routing.Priority != 0 {
			file.Routing.Priority = ptr(provider.Routing.Priority)
		}
		if provider.Routing.InputCostPerMillion != 0 {
			file.Routing.InputCostPerMillion = ptr(provider.Routing.InputCostPerMillion)
		}
		if provider.Routing.OutputCostPerMillion != 0 {
			file.Routing.OutputCostPerMillion = ptr(provider.Routing.OutputCostPerMillion)
		}
	}
	if !reflect.ValueOf(provider.WebTools).IsZero() {
		value := cloneWebTools(provider.WebTools)
		file.WebTools = &value
	}
	if !reflect.ValueOf(provider.WebCapabilities).IsZero() {
		value := provider.WebCapabilities
		file.WebCapabilities = &value
	}
	return file
}

func fileMCPServer(server MCPServerConfig) FileMCPServerConfig {
	file := FileMCPServerConfig{}
	if transport := server.EffectiveTransport(); transport != MCPTransportStdio {
		file.Transport = ptr(transport)
	}
	if !server.IsEnabled() {
		file.Enabled = ptr(false)
	}
	if server.Required {
		file.Required = ptr(true)
	}
	if server.StartupTimeoutSeconds != 0 {
		file.StartupTimeoutSeconds = ptr(server.StartupTimeoutSeconds)
	} else if server.TimeoutSeconds != 0 {
		file.StartupTimeoutSeconds = ptr(server.TimeoutSeconds)
	}
	if server.CallTimeoutSeconds != 0 {
		file.CallTimeoutSeconds = ptr(server.CallTimeoutSeconds)
	}
	if server.AllowTools != nil {
		value := append([]string(nil), server.AllowTools...)
		file.AllowTools = &value
	}
	if server.DenyTools != nil {
		value := append([]string(nil), server.DenyTools...)
		file.DenyTools = &value
	}
	if server.Command != "" {
		file.Command = ptr(server.Command)
	}
	if server.Args != nil {
		value := append([]string(nil), server.Args...)
		file.Args = &value
	}
	if server.Environment != nil {
		value := append([]string(nil), server.Environment...)
		file.Environment = &value
	}
	if server.WorkingDirectory != "" {
		file.WorkingDirectory = ptr(server.WorkingDirectory)
	}
	if server.URL != "" {
		file.URL = ptr(server.URL)
	}
	if server.Headers != nil {
		value := cloneStringMap(server.Headers)
		file.Headers = &value
	}
	if server.EnvironmentHeaders != nil {
		value := cloneStringMap(server.EnvironmentHeaders)
		file.EnvironmentHeaders = &value
	}
	if server.BearerTokenEnvironment != "" {
		file.BearerTokenEnvironment = ptr(server.BearerTokenEnvironment)
	}
	if server.OAuth != nil {
		file.OAuth = fileMCPOAuth(*server.OAuth)
	}
	if server.ReadOnlyTools != nil {
		value := append([]string(nil), server.ReadOnlyTools...)
		file.ReadOnlyTools = &value
	}
	return file
}

func fileMCPOAuth(oauth MCPOAuthConfig) *FileMCPOAuthConfig {
	file := &FileMCPOAuthConfig{}
	if oauth.Issuer != "" {
		file.Issuer = ptr(oauth.Issuer)
	}
	if oauth.ClientID != "" {
		file.ClientID = ptr(oauth.ClientID)
	}
	if oauth.ClientSecretEnvironment != "" {
		file.ClientSecretEnvironment = ptr(oauth.ClientSecretEnvironment)
	}
	if oauth.Scopes != nil {
		value := append([]string(nil), oauth.Scopes...)
		file.Scopes = &value
	}
	if oauth.RedirectURL != "" {
		file.RedirectURL = ptr(oauth.RedirectURL)
	}
	return file
}

func fileSkillRegistry(registry SkillRegistryConfig) FileSkillRegistry {
	file := FileSkillRegistry{}
	if registry.IndexURL != "" {
		file.IndexURL = ptr(registry.IndexURL)
	}
	if registry.PublicKeys != nil {
		keys := cloneStringMap(registry.PublicKeys)
		file.PublicKeys = &keys
	}
	if registry.TokenEnvironment != "" {
		file.TokenEnvironment = ptr(registry.TokenEnvironment)
	}
	if registry.TimeoutSeconds != 0 {
		file.TimeoutSeconds = ptr(registry.TimeoutSeconds)
	}
	if registry.Disabled {
		file.Disabled = ptr(true)
	}
	return file
}

func fileModelMetadata(value, defaults ModelMetadataConfig) *FileModelMetadataConfig {
	file := &FileModelMetadataConfig{}
	setDifferent(&file.Enabled, value.Enabled, defaults.Enabled)
	setDifferent(&file.CatalogURL, value.CatalogURL, defaults.CatalogURL)
	setDifferent(&file.RequestTimeoutSeconds, value.RequestTimeoutSeconds, defaults.RequestTimeoutSeconds)
	setDifferent(&file.EndpointTTLHours, value.EndpointTTLHours, defaults.EndpointTTLHours)
	setDifferent(&file.CatalogTTLHours, value.CatalogTTLHours, defaults.CatalogTTLHours)
	setDifferent(&file.StaleTTLHours, value.StaleTTLHours, defaults.StaleTTLHours)
	setDifferent(&file.NegativeTTLMinutes, value.NegativeTTLMinutes, defaults.NegativeTTLMinutes)
	setDifferent(&file.LearnedTTLHours, value.LearnedTTLHours, defaults.LearnedTTLHours)
	setDifferent(&file.MaxResponseBytes, value.MaxResponseBytes, defaults.MaxResponseBytes)
	setDifferent(&file.MaxCacheEntries, value.MaxCacheEntries, defaults.MaxCacheEntries)
	if !reflect.DeepEqual(value.ProbeTiers, defaults.ProbeTiers) {
		tiers := append([]int(nil), value.ProbeTiers...)
		file.ProbeTiers = &tiers
	}
	if reflect.ValueOf(*file).IsZero() {
		return nil
	}
	return file
}

func applyPatch[T any](target **T, patch ValuePatch[T]) {
	if patch.Remove {
		*target = nil
		return
	}
	if patch.Set {
		value := patch.Value
		*target = &value
	}
}

func assign[T any](_ T, value *T, setter func(T)) {
	if value != nil {
		setter(*value)
	}
}
func assignSlice(value *[]string, setter func([]string)) {
	if value != nil {
		setter(append([]string(nil), (*value)...))
	}
}
func ptr[T any](value T) *T { return &value }
func setDifferent[T comparable](target **T, value, defaults T) {
	if value != defaults {
		*target = ptr(value)
	}
}
func setSliceDifferent(target **[]string, value, defaults []string) {
	if !reflect.DeepEqual(value, defaults) {
		copy := append([]string(nil), value...)
		*target = &copy
	}
}

func addName(values []string, name string) []string {
	values = removeName(values, name)
	values = append(values, name)
	sort.Strings(values)
	return values
}

func removeName(values []string, name string) []string {
	result := values[:0]
	for _, value := range values {
		if value != name {
			result = append(result, value)
		}
	}
	return result
}
