package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
	"Eylu/internal/webtool"
)

func (c *Conversation) refreshMCPRuntime(runtime Runtime, executor *tool.Executor, baseTools []tool.Tool) (Runtime, error) {
	live := MCPRuntimeState{}
	if runtime.MCPState != nil {
		live = runtime.MCPState()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.applyMCPRuntimeLocked(runtime, executor, baseTools, live)
}

func (c *Conversation) applyMCPRuntimeLocked(runtime Runtime, executor *tool.Executor, baseTools []tool.Tool, live MCPRuntimeState) (Runtime, error) {
	state := MCPRuntimeState{}
	if runtime.MCPState != nil {
		state = filterMCPRuntimeState(live, runtime.PermissionMode)
		if c.profile != nil {
			filtered := state.Tools[:0]
			servers := make(map[string]string)
			for _, item := range state.Tools {
				definition := item.Definition()
				if !c.profile.AllowsTool(definition.Name, item.Risk()) {
					continue
				}
				filtered = append(filtered, item)
				if server := state.ToolServers[definition.Name]; server != "" {
					servers[definition.Name] = server
				}
			}
			state.Tools = filtered
			state.ToolServers = servers
		}
	}
	registry := tool.NewRegistry()
	for _, item := range append(append([]tool.Tool(nil), baseTools...), state.Tools...) {
		if err := registry.Register(item); err != nil {
			return runtime, fmt.Errorf("refresh MCP tool registry: %w", err)
		}
	}
	executor.Registry = registry
	runtime.MCPContexts = state.Contexts
	runtime.MCPToolServers = state.ToolServers
	runtime.MCPFingerprint = state.Fingerprint
	if err := c.applyRuntime(runtime); err != nil {
		return runtime, err
	}
	c.rebuildLedger(runtime)
	return runtime, nil
}

// applyToolDefinitions records the tool set of the current round and rebuilds the
// ledger under the state lock.
func (c *Conversation) applyToolDefinitions(runtime Runtime, definitions []protocol.ToolDefinition) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toolDefinitions = append(c.toolDefinitions[:0], definitions...)
	c.rebuildLedger(runtime)
}

func (c *Conversation) resolveWebRuntime(runtime Runtime, executor *tool.Executor, budget *webtool.UsageBudget) (webtool.ResolvedWebToolPlan, error) {
	functionTools := executor.Definitions()
	clientTools := make(map[string]protocol.ToolDefinition, len(functionTools))
	clientItems := make(map[string]tool.Tool, len(functionTools))
	for _, definition := range functionTools {
		clientTools[definition.Name] = definition
		if item, ok := executor.Registry.Get(definition.Name); ok {
			clientItems[definition.Name] = item
		}
	}
	plan, err := webtool.Resolve(webtool.PlanInput{
		ProviderName:  runtime.Provider.Name,
		Provider:      runtime.Provider.Config,
		Capabilities:  driver.CapabilitiesFor(runtime.Driver, capabilityTarget(runtime)),
		FunctionTools: functionTools,
		ClientTools:   clientTools,
	})
	if err != nil {
		return webtool.ResolvedWebToolPlan{}, err
	}
	for _, resolved := range plan.Local {
		item := webtool.NewLocalTool(resolved, clientItems[resolved.Target], runtime.WebDelegate, runtime.Provider.Config.WebTools.Permission, budget)
		if err := executor.Registry.Register(item); err != nil {
			return webtool.ResolvedWebToolPlan{}, fmt.Errorf("register %s fallback: %w", resolved.Definition.Kind, err)
		}
	}
	return plan, nil
}

func capabilityTarget(runtime Runtime) driver.CapabilityTarget {
	return driver.CapabilityTarget{
		Provider: runtime.Provider.Config.CatalogProvider,
		Protocol: runtime.Provider.Config.Adapter,
		Model:    runtime.Provider.Config.Model,
	}
}

func authorizeHostedWeb(ctx context.Context, executor *tool.Executor, runtime Runtime, plan webtool.ResolvedWebToolPlan, authorized *bool) error {
	if len(plan.Hosted) == 0 || *authorized {
		return nil
	}
	permission := strings.ToLower(strings.TrimSpace(runtime.Provider.Config.WebTools.Permission))
	if permission == "" {
		permission = "allow"
	}
	if permission == "allow" {
		*authorized = true
		return nil
	}
	if permission == "deny" {
		return &protocol.Error{Code: protocol.ErrTool, Message: "hosted web tools are denied by policy"}
	}
	if executor.Confirm == nil {
		return &protocol.Error{Code: protocol.ErrTool, Message: "hosted web tools require approval"}
	}
	kinds := make([]string, 0, len(plan.Hosted))
	for _, hosted := range plan.Hosted {
		kinds = append(kinds, string(hosted.Definition.Kind))
	}
	input, _ := json.Marshal(map[string]any{"provider": runtime.Provider.Name, "tools": kinds})
	request := policy.Request{Tool: "hosted_web", Input: input, Workspace: executor.Workspace, Risk: policy.RiskNetwork, ConfirmationStep: 1, ConfirmationTotal: 1}
	mode, _ := policy.ParseMode(runtime.PermissionMode)
	outcome := policy.Outcome{Mode: mode, Risk: policy.RiskNetwork, Decision: policy.DecisionConfirm, Confirmations: 1, Classification: policy.CommandNotApplicable, Reason: "hosted web access"}
	confirmation, err := executor.Confirm(ctx, request, outcome)
	if err != nil {
		return err
	}
	if !confirmation.Approved {
		message := strings.TrimSpace(confirmation.RejectionReason)
		if message == "" {
			message = "hosted web access was rejected"
		}
		return &protocol.Error{Code: protocol.ErrTool, Message: message}
	}
	*authorized = true
	return nil
}

func registryToolsExcluding(registry *tool.Registry, excluded map[string]string) []tool.Tool {
	if registry == nil {
		return nil
	}
	definitions := registry.Definitions()
	items := make([]tool.Tool, 0, len(definitions))
	for _, definition := range definitions {
		if excluded[definition.Name] != "" {
			continue
		}
		if item, ok := registry.Get(definition.Name); ok {
			if _, dynamicWebTool := item.(*webtool.LocalTool); dynamicWebTool {
				continue
			}
			items = append(items, item)
		}
	}
	return items
}
