package webtool

import (
	"encoding/json"
	"testing"

	"Eylu/internal/config"
	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

func TestResolveAutoHostedAndStableToolOrder(t *testing.T) {
	provider := config.ProviderConfig{CatalogProvider: "openai", WebTools: config.WebToolsConfig{Permission: config.WebPermissionAllow}}
	plan, err := Resolve(PlanInput{
		ProviderName: "work", Provider: provider,
		Capabilities:  driver.Capabilities{HostedWebSearch: true, HostedWebFetch: true, HostedAndFunctionTools: true, SearchDomainFilter: true, SearchLocation: true},
		FunctionTools: []protocol.ToolDefinition{{Name: "z_tool", InputSchema: json.RawMessage(`{"type":"object"}`)}, {Name: "a_tool", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Definitions) != 4 || plan.Definitions[0].Name != "a_tool" || plan.Definitions[2].Kind != protocol.ToolWebFetch || plan.Definitions[3].Kind != protocol.ToolWebSearch {
		t.Fatalf("stable definitions = %#v", plan.Definitions)
	}
	if len(plan.Hosted) != 2 || len(plan.Local) != 0 {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestResolveExplicitUnsupportedToolAndImplicitOmission(t *testing.T) {
	provider := config.ProviderConfig{WebTools: config.WebToolsConfig{Permission: config.WebPermissionAllow}}
	plan, err := Resolve(PlanInput{ProviderName: "work", Provider: provider})
	if err != nil || len(plan.Definitions) != 0 {
		t.Fatalf("implicit unsupported plan=%#v error=%v", plan, err)
	}
	enabled := true
	provider.WebTools.Search.Enabled = &enabled
	_, err = Resolve(PlanInput{ProviderName: "work", Provider: provider})
	if typed, ok := err.(*protocol.Error); !ok || typed.Code != protocol.ErrUnsupportedTool {
		t.Fatalf("error = %#v", err)
	}
}

func TestResolveMixedToolsUsesClientFallback(t *testing.T) {
	enabled := true
	provider := config.ProviderConfig{WebTools: config.WebToolsConfig{
		Permission: config.WebPermissionAllow,
		Search:     config.WebToolConfig{Enabled: &enabled, Execution: "auto", Fallback: "client", ClientTool: "mcp__web__search"},
	}}
	plan, err := Resolve(PlanInput{
		ProviderName: "work", Provider: provider,
		Capabilities:  driver.Capabilities{HostedWebSearch: true, HostedAndFunctionTools: false},
		FunctionTools: []protocol.ToolDefinition{{Name: "read_file"}},
		ClientTools:   map[string]protocol.ToolDefinition{"mcp__web__search": {Name: "mcp__web__search", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Local) != 1 || plan.Local[0].Execution != protocol.ExecutionClient || plan.Local[0].Target != "mcp__web__search" {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestApplyCapabilityOverrides(t *testing.T) {
	value := false
	caps := ApplyCapabilityOverrides(driver.Capabilities{HostedWebSearch: true, HostedAndFunctionTools: true}, config.WebCapabilityOverrides{HostedWebSearch: &value})
	if caps.HostedWebSearch || !caps.HostedAndFunctionTools {
		t.Fatalf("capabilities = %#v", caps)
	}
}
