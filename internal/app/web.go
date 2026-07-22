package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"Eylu/internal/config"
	"Eylu/internal/driver"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/webtool"
)

func (r *runtime) delegatedWebBackend(manager *provider.Manager) webtool.DelegateFunc {
	return func(ctx context.Context, resolved webtool.ResolvedTool, input json.RawMessage) protocol.ToolResult {
		snapshot, ok := manager.Snapshot(resolved.Target)
		if !ok {
			return delegatedWebError(resolved, fmt.Errorf("provider %q does not exist", resolved.Target))
		}
		if snapshot.Config.WebTools.Permission == config.WebPermissionDeny {
			return delegatedWebError(resolved, fmt.Errorf("provider %q denies web access", resolved.Target))
		}
		timeout := snapshot.Config.Timeout(60 * time.Second)
		modelDriver, err := defaultDriverRegistry(&http.Client{Timeout: timeout}).Get(snapshot.Config.Adapter)
		if err != nil {
			return delegatedWebError(resolved, err)
		}
		target := driver.CapabilityTarget{Provider: snapshot.Config.CatalogProvider, Protocol: snapshot.Config.Adapter, Model: snapshot.Config.Model}
		capabilities := webtool.ApplyCapabilityOverrides(driver.CapabilitiesFor(modelDriver, target), snapshot.Config.WebCapabilities)
		if err := validateDelegatedCapability(resolved.Definition, capabilities); err != nil {
			return delegatedWebError(resolved, err)
		}
		prompt, err := delegatedWebPrompt(resolved.Definition.Kind, input)
		if err != nil {
			return delegatedWebError(resolved, err)
		}
		definition := resolved.Definition
		definition.Execution = protocol.ExecutionHosted
		definition.Fallback = ""
		definition.InputSchema = nil
		response, err := modelDriver.Generate(ctx, driver.Request{
			BaseURL: snapshot.Config.BaseURL, APIKey: providerAPIKey(snapshot.Config), Headers: snapshot.Config.Headers,
			ReasoningEffort: snapshot.Config.ReasoningEffort, Target: target,
			Model: protocol.ModelRequest{
				ProtocolVersion: protocol.Version, Model: snapshot.Config.Model, Tools: []protocol.ToolDefinition{definition},
				Turns: []protocol.Turn{{ID: uuid.NewString(), Role: protocol.RoleUser, CreatedAt: time.Now().UTC(), Parts: []protocol.Part{{Kind: protocol.PartText, Text: prompt}}}},
			},
		}, nil)
		if err != nil {
			return delegatedWebError(resolved, err)
		}
		if response.Stop == protocol.StopToolUse {
			return delegatedWebError(resolved, fmt.Errorf("provider %q requested a local tool during delegated web execution", resolved.Target))
		}
		return delegatedWebResult(resolved, response)
	}
}

func validateDelegatedCapability(definition protocol.ToolDefinition, capabilities driver.Capabilities) error {
	supported := capabilities.HostedWebSearch
	if definition.Kind == protocol.ToolWebFetch {
		supported = capabilities.HostedWebFetch
	}
	if !supported {
		return &protocol.Error{Code: protocol.ErrUnsupportedTool, Message: fmt.Sprintf("delegated provider lacks %s capability", definition.Kind)}
	}
	if (len(definition.AllowedDomains) > 0 || len(definition.BlockedDomains) > 0) && !capabilities.SearchDomainFilter {
		return &protocol.Error{Code: protocol.ErrUnsupportedTool, Message: "delegated provider lacks domain filtering capability"}
	}
	if definition.UserLocation != nil && !capabilities.SearchLocation {
		return &protocol.Error{Code: protocol.ErrUnsupportedTool, Message: "delegated provider lacks location capability"}
	}
	return nil
}

func delegatedWebPrompt(kind protocol.ToolKind, input json.RawMessage) (string, error) {
	var values map[string]any
	if err := json.Unmarshal(input, &values); err != nil {
		return "", fmt.Errorf("decode delegated web input: %w", err)
	}
	field, prefix := "query", "Search the web for: "
	if kind == protocol.ToolWebFetch {
		field, prefix = "url", "Fetch this URL: "
	}
	value, _ := values[field].(string)
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("delegated web input requires %q", field)
	}
	return prefix + value, nil
}

func delegatedWebResult(resolved webtool.ResolvedTool, response protocol.ModelResponse) protocol.ToolResult {
	var text strings.Builder
	activities := make([]protocol.WebActivity, 0)
	citations := make([]protocol.URLCitation, 0)
	for _, part := range response.Turn.Parts {
		switch {
		case part.Kind == protocol.PartText:
			text.WriteString(part.Text)
		case part.Kind == protocol.PartWebActivity && part.WebActivity != nil:
			activities = append(activities, *part.WebActivity)
		case part.Kind == protocol.PartCitation && part.Citation != nil:
			citations = append(citations, *part.Citation)
		}
	}
	structured, _ := json.Marshal(map[string]any{"activities": activities, "citations": citations, "usage": response.Usage})
	return protocol.ToolResult{
		Content: strings.TrimSpace(text.String()), StructuredContent: structured,
		Metadata: map[string]any{
			"web_backend": string(protocol.ExecutionDelegated), "web_kind": string(resolved.Definition.Kind),
			"web_target": resolved.Target, "web_status": string(protocol.WebStatusCompleted), "citation_count": len(citations), "activity_count": len(activities),
			"web_input_tokens": response.Usage.InputTokens, "web_output_tokens": response.Usage.OutputTokens, "web_cost_usd": webActivityCost(activities),
			"untrusted_web_content": true,
		},
	}
}

func webActivityCost(activities []protocol.WebActivity) float64 {
	var total float64
	for _, activity := range activities {
		total += activity.Usage.CostUSD
	}
	return total
}

func delegatedWebError(resolved webtool.ResolvedTool, err error) protocol.ToolResult {
	return protocol.ToolResult{Content: err.Error(), IsError: true, Metadata: map[string]any{
		"web_backend": string(protocol.ExecutionDelegated), "web_kind": string(resolved.Definition.Kind),
		"web_target": resolved.Target, "untrusted_web_content": true,
	}}
}
