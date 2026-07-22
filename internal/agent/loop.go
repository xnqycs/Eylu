package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
	"Eylu/internal/webtool"
)

type LoopOptions struct {
	MaxTurns       int
	MaxTotalTokens int
	RequestID      string
}

var ErrRequestInterrupted = errors.New("request interrupted by user")

func (c *Conversation) Run(ctx context.Context, prompt string, runtime Runtime, executor *tool.Executor, options LoopOptions, stream bool, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.prepareRuntime(prompt, runtime); err != nil {
		return protocol.ModelResponse{}, err
	}
	if executor == nil {
		return protocol.ModelResponse{}, fmt.Errorf("tool executor is nil")
	}
	baseTools := registryToolsExcluding(executor.Registry, runtime.MCPToolServers)
	webBudget := webtool.NewUsageBudget()
	hostedAuthorized := false
	var err error
	runtime, err = c.refreshMCPRuntime(runtime, executor, baseTools)
	if err != nil {
		return protocol.ModelResponse{}, err
	}
	maxTurns := options.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 20
	}
	c.appendUser(prompt)
	plan, err := c.resolveWebRuntime(runtime, executor, webBudget)
	if err != nil {
		return protocol.ModelResponse{}, err
	}
	if err := authorizeHostedWeb(ctx, executor, runtime, plan, &hostedAuthorized); err != nil {
		return protocol.ModelResponse{}, err
	}
	definitions := plan.Definitions
	c.toolDefinitions = append(c.toolDefinitions[:0], definitions...)
	c.rebuildLedger(runtime)
	seenCalls := make(map[string]struct{})
	totalTokens := 0
	requestID := options.RequestID
	if requestID == "" {
		requestID = uuid.NewString()
	}
	var last protocol.ModelResponse
	for iteration := 0; iteration < maxTurns; iteration++ {
		if err := ctx.Err(); err != nil {
			return protocol.ModelResponse{}, err
		}
		runtime, err = c.refreshMCPRuntime(runtime, executor, baseTools)
		if err != nil {
			return protocol.ModelResponse{}, err
		}
		plan, err = c.resolveWebRuntime(runtime, executor, webBudget)
		if err != nil {
			return protocol.ModelResponse{}, err
		}
		if err := authorizeHostedWeb(ctx, executor, runtime, plan, &hostedAuthorized); err != nil {
			return protocol.ModelResponse{}, err
		}
		definitions = plan.Definitions
		c.toolDefinitions = append(c.toolDefinitions[:0], definitions...)
		c.rebuildLedger(runtime)
		parallelToolCalls := executor.ParallelLimit() > 1 && driver.CapabilitiesFor(runtime.Driver, capabilityTarget(runtime)).ParallelTools
		response, err := c.generate(ctx, runtime, definitions, parallelToolCalls, stream, emit)
		if err != nil {
			return protocol.ModelResponse{}, err
		}
		last = response
		if err := recordHostedWebActivities(executor, runtime, requestID, response.Turn, plan, webBudget); err != nil {
			return last, err
		}
		totalTokens += response.Usage.InputTokens + response.Usage.OutputTokens
		if options.MaxTotalTokens > 0 && totalTokens > options.MaxTotalTokens {
			return last, &protocol.Error{Code: protocol.ErrProtocol, Message: "agent token budget exceeded"}
		}
		if response.Stop != protocol.StopToolUse {
			return response, nil
		}
		calls := toolCalls(response.Turn)
		if len(calls) == 0 {
			return last, &protocol.Error{Code: protocol.ErrProtocol, Message: "model stopped for tool use without tool calls"}
		}
		toolTurn := protocol.Turn{ID: uuid.NewString(), Role: protocol.RoleTool, CreatedAt: time.Now().UTC()}
		interrupted := false
		for _, call := range calls {
			if call.ID == "" {
				return last, &protocol.Error{Code: protocol.ErrProtocol, Message: "model returned a tool call without an ID"}
			}
			if _, duplicate := seenCalls[call.ID]; duplicate {
				return last, &protocol.Error{Code: protocol.ErrProtocol, Message: fmt.Sprintf("duplicate tool call ID %q", call.ID)}
			}
			seenCalls[call.ID] = struct{}{}
		}
		runtime, err = c.refreshMCPRuntime(runtime, executor, baseTools)
		if err != nil {
			return last, err
		}
		plan, err = c.resolveWebRuntime(runtime, executor, webBudget)
		if err != nil {
			return last, err
		}
		definitions = plan.Definitions
		c.toolDefinitions = append(c.toolDefinitions[:0], definitions...)
		c.rebuildLedger(runtime)
		hooks := tool.BatchHooks{}
		if emit != nil {
			hooks.OnStart = func(call protocol.ToolCall) error {
				return emit(protocol.ModelEvent{Kind: protocol.EventToolStart, ToolCall: &call})
			}
			hooks.OnResult = func(result protocol.ToolResult) error {
				return emit(protocol.ModelEvent{Kind: protocol.EventToolResult, ToolResult: &result})
			}
		}
		results, batchErr := executor.ExecuteBatch(ctx, requestID, calls, hooks)
		for index := range results {
			result := results[index]
			if result.Metadata != nil && result.Metadata["interrupt_request"] == true {
				interrupted = true
			}
			c.captureSkillResult(result)
			c.captureTodoListResult(result)
			toolTurn.Parts = append(toolTurn.Parts, protocol.Part{Kind: protocol.PartToolResult, ToolResult: &result})
		}
		c.turns = append(c.turns, toolTurn)
		c.projectMapDirty = true
		if interrupted {
			c.driverState = nil
		}
		c.rebuildLedger(runtime)
		if batchErr != nil {
			return last, batchErr
		}
		if interrupted {
			return last, ErrRequestInterrupted
		}
	}
	return last, &protocol.Error{Code: protocol.ErrProtocol, Message: fmt.Sprintf("agent iteration limit exceeded (%d)", maxTurns)}
}

func recordHostedWebActivities(executor *tool.Executor, runtime Runtime, requestID string, turn protocol.Turn, plan webtool.ResolvedWebToolPlan, budget *webtool.UsageBudget) error {
	limits := make(map[protocol.ToolKind]int, len(plan.Hosted))
	for _, hosted := range plan.Hosted {
		limits[hosted.Definition.Kind] = hosted.Definition.MaxUses
	}
	citations := make(map[string]int)
	for _, part := range turn.Parts {
		if part.Kind == protocol.PartCitation && part.Citation != nil {
			citations[part.Citation.CallID]++
		}
	}
	for _, part := range turn.Parts {
		if part.Kind != protocol.PartWebActivity || part.WebActivity == nil {
			continue
		}
		activity := part.WebActivity
		if !budget.Record(activity.Kind, limits[activity.Kind]) {
			return &protocol.Error{Code: protocol.ErrTool, Message: fmt.Sprintf("%s max_uses exceeded", activity.Kind)}
		}
		if executor.Audit == nil {
			continue
		}
		inputBytes := len([]byte(activity.Query)) + len([]byte(activity.URL))
		executor.Audit.Record(tool.AuditRecord{
			Timestamp: time.Now().UTC(), RequestID: requestID, SessionID: executor.SessionID,
			ProviderName: runtime.Provider.Name, ProviderGeneration: runtime.Provider.Generation, Model: runtime.Provider.Config.Model,
			CallID: activity.CallID, Tool: string(activity.Kind), Risk: policy.RiskNetwork, Decision: policy.DecisionAllow,
			Reason: "hosted web execution", Confirmed: runtime.Provider.Config.WebTools.Permission == "ask", DurationMS: activity.DurationMS,
			ExecutionDurationMS: activity.DurationMS, IsError: activity.Status == protocol.WebStatusError, InputBytes: inputBytes,
			Mode: runtime.PermissionMode, Classification: policy.CommandNotApplicable, WebBackend: string(protocol.ExecutionHosted),
			WebStatus: string(activity.Status), WebSources: max(len(activity.Sources), citations[activity.CallID]),
			WebInputTokens: activity.Usage.InputTokens, WebOutputTokens: activity.Usage.OutputTokens, WebCostUSD: activity.Usage.CostUSD,
			UntrustedWebContent: true,
		})
	}
	return nil
}

func (c *Conversation) refreshMCPRuntime(runtime Runtime, executor *tool.Executor, baseTools []tool.Tool) (Runtime, error) {
	state := MCPRuntimeState{}
	if runtime.MCPState != nil {
		state = filterMCPRuntimeState(runtime.MCPState(), runtime.PermissionMode)
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
		permission = "ask"
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
			items = append(items, item)
		}
	}
	return items
}

func (c *Conversation) captureTodoListResult(result protocol.ToolResult) {
	if result.IsError || result.TodoList == nil {
		return
	}
	c.todoList = cloneTodoList(*result.TodoList)
}

func toolCalls(turn protocol.Turn) []protocol.ToolCall {
	result := make([]protocol.ToolCall, 0)
	for _, part := range turn.Parts {
		if part.Kind == protocol.PartToolCall && part.ToolCall != nil {
			result = append(result, *part.ToolCall)
		}
	}
	return result
}
