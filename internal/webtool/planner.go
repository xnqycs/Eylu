package webtool

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"Eylu/internal/config"
	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

type PlanInput struct {
	ProviderName  string
	Provider      config.ProviderConfig
	Capabilities  driver.Capabilities
	FunctionTools []protocol.ToolDefinition
	ClientTools   map[string]protocol.ToolDefinition
}

type ResolvedTool struct {
	Definition protocol.ToolDefinition
	Execution  protocol.ToolExecution
	Target     string
}

type ResolvedWebToolPlan struct {
	Definitions []protocol.ToolDefinition
	Hosted      []ResolvedTool
	Local       []ResolvedTool
}

const MaxBatchQueries = 10

func Resolve(input PlanInput) (ResolvedWebToolPlan, error) {
	input.Capabilities = ApplyCapabilityOverrides(input.Capabilities, input.Provider.WebCapabilities)
	plan := ResolvedWebToolPlan{Definitions: append([]protocol.ToolDefinition(nil), input.FunctionTools...)}
	if input.Provider.WebTools.Permission == config.WebPermissionDeny {
		sortDefinitions(plan.Definitions)
		return plan, nil
	}
	webTools := []struct {
		kind protocol.ToolKind
		cfg  config.WebToolConfig
	}{
		{kind: protocol.ToolWebSearch, cfg: input.Provider.WebTools.Search},
		{kind: protocol.ToolWebFetch, cfg: input.Provider.WebTools.Fetch},
	}
	for _, candidate := range webTools {
		resolved, include, err := resolveOne(input, candidate.kind, candidate.cfg, len(input.FunctionTools) > 0)
		if err != nil {
			return ResolvedWebToolPlan{}, err
		}
		if !include {
			continue
		}
		plan.Definitions = append(plan.Definitions, resolved.Definition)
		if resolved.Execution == protocol.ExecutionHosted {
			plan.Hosted = append(plan.Hosted, resolved)
		} else {
			plan.Local = append(plan.Local, resolved)
		}
	}
	sortDefinitions(plan.Definitions)
	sortResolved(plan.Hosted)
	sortResolved(plan.Local)
	return plan, nil
}

func resolveOne(input PlanInput, kind protocol.ToolKind, cfg config.WebToolConfig, hasFunctions bool) (ResolvedTool, bool, error) {
	explicit := cfg.Enabled != nil && *cfg.Enabled
	if cfg.Enabled != nil && !*cfg.Enabled {
		return ResolvedTool{}, false, nil
	}
	definition, err := definitionFor(kind, cfg)
	if err != nil {
		return ResolvedTool{}, false, err
	}
	execution := protocol.ToolExecution(cfg.Execution).Effective()
	selfDelegated := execution == protocol.ExecutionAuto && prefersClientWebFanout(input.Provider, kind)
	if execution == protocol.ExecutionAuto {
		if selfDelegated && hostedSupported(kind, cfg, input.Capabilities, false) {
			execution = protocol.ExecutionDelegated
		} else if hostedSupported(kind, cfg, input.Capabilities, hasFunctions) {
			execution = protocol.ExecutionHosted
		} else if cfg.Fallback != "" {
			execution = protocol.ToolExecution(cfg.Fallback)
		} else if explicit {
			return ResolvedTool{}, false, unsupported(kind, input.ProviderName, "hosted capability is unavailable and no fallback is configured")
		} else {
			return ResolvedTool{}, false, nil
		}
	}
	resolved := ResolvedTool{Definition: definition, Execution: execution}
	resolved.Definition.Execution = execution
	if execution == protocol.ExecutionClient || execution == protocol.ExecutionDelegated {
		resolved.Definition.InputSchema = canonicalInputSchema(kind)
	}
	switch execution {
	case protocol.ExecutionHosted:
		if !hostedSupported(kind, cfg, input.Capabilities, hasFunctions) {
			return ResolvedTool{}, false, unsupported(kind, input.ProviderName, "hosted capability does not satisfy the request")
		}
	case protocol.ExecutionClient:
		resolved.Target = strings.TrimSpace(cfg.ClientTool)
		client, ok := input.ClientTools[resolved.Target]
		if resolved.Target == "" || !ok {
			return ResolvedTool{}, false, unsupported(kind, input.ProviderName, "configured MCP client tool is unavailable")
		}
		if kind == protocol.ToolWebFetch && !cfg.TrustedNetworkBoundary {
			return ResolvedTool{}, false, unsupported(kind, input.ProviderName, "MCP fetch requires trusted_network_boundary")
		}
		if err := validateClientSchema(kind, client.InputSchema); err != nil {
			return ResolvedTool{}, false, unsupported(kind, input.ProviderName, err.Error())
		}
	case protocol.ExecutionDelegated:
		resolved.Target = strings.TrimSpace(cfg.DelegatedProvider)
		if resolved.Target == "" && selfDelegated {
			resolved.Target = input.ProviderName
		}
		if resolved.Target == "" {
			return ResolvedTool{}, false, unsupported(kind, input.ProviderName, "delegated provider is required")
		}
	default:
		return ResolvedTool{}, false, unsupported(kind, input.ProviderName, fmt.Sprintf("execution %q is invalid", execution))
	}
	return resolved, true, nil
}

func definitionFor(kind protocol.ToolKind, cfg config.WebToolConfig) (protocol.ToolDefinition, error) {
	definition := protocol.ToolDefinition{
		Kind: kind, Name: string(kind), Execution: protocol.ToolExecution(cfg.Execution).Effective(),
		ToolChoice: protocol.ToolChoiceAuto, Fallback: protocol.ToolExecution(cfg.Fallback),
		AllowedDomains: append([]string(nil), cfg.AllowedDomains...), BlockedDomains: append([]string(nil), cfg.BlockedDomains...),
		MaxUses: cfg.MaxUses, ContextSize: protocol.WebContextSize(cfg.ContextSize).Effective(),
	}
	if definition.MaxUses == 0 {
		definition.MaxUses = 5
	}
	if kind == protocol.ToolWebSearch {
		definition.Description = "Search the public web and return cited sources. Put two or more independent searches in queries so they execute concurrently in one tool round."
	} else {
		definition.Description = "Fetch an allowed public web page and return cited content."
	}
	if cfg.Country != "" || cfg.Region != "" || cfg.City != "" || cfg.Timezone != "" {
		definition.UserLocation = &protocol.UserLocation{Country: cfg.Country, Region: cfg.Region, City: cfg.City, Timezone: cfg.Timezone}
	}
	if len(cfg.ProviderOptions) > 0 {
		definition.ProviderOptions = make(map[string]json.RawMessage, len(cfg.ProviderOptions))
		for key, value := range cfg.ProviderOptions {
			encoded, err := json.Marshal(value)
			if err != nil {
				return protocol.ToolDefinition{}, &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("encode web provider option %q", key), Cause: err}
			}
			definition.ProviderOptions[key] = encoded
		}
	}
	if definition.Execution == protocol.ExecutionClient || definition.Execution == protocol.ExecutionDelegated || definition.Fallback != "" {
		property := "query"
		if kind == protocol.ToolWebFetch {
			property = "url"
		}
		definition.InputSchema = json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"%s":{"type":"string"}},"required":["%s"],"additionalProperties":false}`, property, property))
	}
	return definition, nil
}

func canonicalInputSchema(kind protocol.ToolKind) json.RawMessage {
	if kind == protocol.ToolWebSearch {
		return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"query":{"type":"string","minLength":1},"queries":{"type":"array","items":{"type":"string","minLength":1},"minItems":1,"maxItems":%d}},"oneOf":[{"required":["query"]},{"required":["queries"]}],"additionalProperties":false}`, MaxBatchQueries))
	}
	return json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","minLength":1}},"required":["url"],"additionalProperties":false}`)
}

func prefersClientWebFanout(provider config.ProviderConfig, kind protocol.ToolKind) bool {
	if kind != protocol.ToolWebSearch || !strings.EqualFold(strings.TrimSpace(provider.Adapter), "openai_responses") || strings.TrimSpace(provider.CatalogProvider) != "" {
		return false
	}
	model := strings.ToLower(strings.TrimSpace(provider.Model))
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		model = model[slash+1:]
	}
	if !strings.HasPrefix(model, "gpt-") && !strings.HasPrefix(model, "chatgpt-") {
		return false
	}
	parsed, err := url.Parse(strings.TrimSpace(provider.BaseURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host != "" && host != "api.openai.com"
}

func hostedSupported(kind protocol.ToolKind, cfg config.WebToolConfig, capabilities driver.Capabilities, hasFunctions bool) bool {
	supported := capabilities.HostedWebSearch
	if kind == protocol.ToolWebFetch {
		supported = capabilities.HostedWebFetch
	}
	if !supported || (hasFunctions && !capabilities.HostedAndFunctionTools) {
		return false
	}
	if (len(cfg.AllowedDomains) > 0 || len(cfg.BlockedDomains) > 0) && !capabilities.SearchDomainFilter {
		return false
	}
	if (cfg.Country != "" || cfg.Region != "" || cfg.City != "" || cfg.Timezone != "") && !capabilities.SearchLocation {
		return false
	}
	return true
}

func validateClientSchema(kind protocol.ToolKind, raw json.RawMessage) error {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &schema) != nil {
		return fmt.Errorf("configured MCP client tool has an invalid input schema")
	}
	property := "query"
	if kind == protocol.ToolWebFetch {
		property = "url"
	}
	if _, ok := schema.Properties[property]; !ok {
		return fmt.Errorf("configured MCP client tool must expose a %q property", property)
	}
	return nil
}

func ApplyCapabilityOverrides(caps driver.Capabilities, overrides config.WebCapabilityOverrides) driver.Capabilities {
	apply := func(target *bool, override *bool) {
		if override != nil {
			*target = *override
		}
	}
	apply(&caps.HostedWebSearch, overrides.HostedWebSearch)
	apply(&caps.HostedWebFetch, overrides.HostedWebFetch)
	apply(&caps.HostedToolStreaming, overrides.HostedToolStreaming)
	apply(&caps.HostedAndFunctionTools, overrides.HostedAndFunctionTools)
	apply(&caps.SearchDomainFilter, overrides.SearchDomainFilter)
	apply(&caps.SearchLocation, overrides.SearchLocation)
	apply(&caps.SearchUsageDetails, overrides.SearchUsageDetails)
	return caps
}

func unsupported(kind protocol.ToolKind, provider, reason string) error {
	return &protocol.Error{Code: protocol.ErrUnsupportedTool, Message: fmt.Sprintf("%s is unavailable for provider %q: %s", kind, provider, reason)}
}

func sortDefinitions(definitions []protocol.ToolDefinition) {
	sort.SliceStable(definitions, func(i, j int) bool {
		left, right := definitions[i].Kind.Effective(), definitions[j].Kind.Effective()
		if left != right {
			return left < right
		}
		return definitions[i].Name < definitions[j].Name
	})
}

func sortResolved(tools []ResolvedTool) {
	sort.SliceStable(tools, func(i, j int) bool { return tools[i].Definition.Name < tools[j].Definition.Name })
}
