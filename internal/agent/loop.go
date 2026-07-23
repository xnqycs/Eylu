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
	BeforeModel    func() string
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
		if options.BeforeModel != nil {
			if message := strings.TrimSpace(options.BeforeModel()); message != "" {
				c.appendUser(message)
				c.rebuildLedger(runtime)
			}
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
		expansion := expandToolCalls(calls, plan)
		hooks := tool.BatchHooks{}
		if emit != nil {
			hooks.OnStart = func(call protocol.ToolCall) error {
				if info, ok := expansion.web[call.ID]; ok {
					activity := webActivityForCall(call, info)
					return emit(protocol.ModelEvent{Kind: webStartedEvent(info.kind), WebActivity: &activity})
				}
				return emit(protocol.ModelEvent{Kind: protocol.EventToolStart, ToolCall: &call})
			}
			hooks.OnResult = func(result protocol.ToolResult) error {
				if info, ok := expansion.web[result.CallID]; ok {
					parts := webPartsForResult(info, result)
					for index := range parts {
						part := &parts[index]
						switch {
						case part.WebActivity != nil:
							if part.WebActivity.CallID != info.callID {
								started := *part.WebActivity
								started.Status = protocol.WebStatusRunning
								started.Error = ""
								if err := emit(protocol.ModelEvent{Kind: webStartedEvent(started.Kind), WebActivity: &started}); err != nil {
									return err
								}
							}
							if err := emit(protocol.ModelEvent{Kind: webCompletedEvent(part.WebActivity.Kind), WebActivity: part.WebActivity}); err != nil {
								return err
							}
						case part.Citation != nil:
							if err := emit(protocol.ModelEvent{Kind: protocol.EventCitation, Citation: part.Citation}); err != nil {
								return err
							}
						}
					}
					return nil
				}
				return emit(protocol.ModelEvent{Kind: protocol.EventToolResult, ToolResult: &result})
			}
		}
		limit := executor.ParallelLimit()
		if len(expansion.web) > limit {
			limit = min(len(expansion.web), webtool.MaxBatchQueries)
		}
		executedResults, batchErr := executor.ExecuteBatchWithLimit(ctx, requestID, expansion.calls, hooks, limit)
		results, webParts := collapseToolResults(calls, expansion, executedResults)
		for index := range results {
			result := results[index]
			if result.Metadata != nil && result.Metadata["interrupt_request"] == true {
				interrupted = true
			}
			c.captureSkillResult(result)
			c.captureTodoListResult(result)
			toolTurn.Parts = append(toolTurn.Parts, protocol.Part{Kind: protocol.PartToolResult, ToolResult: &result})
		}
		toolTurn.Parts = append(toolTurn.Parts, webParts...)
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
		inputBytes := webActivityInputBytes(activity)
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

func webActivityInputBytes(activity *protocol.WebActivity) int {
	if activity == nil {
		return 0
	}
	values := append([]string(nil), activity.Queries...)
	query := strings.TrimSpace(activity.Query)
	found := false
	for _, value := range values {
		if strings.TrimSpace(value) == query {
			found = true
			break
		}
	}
	if query != "" && !found {
		values = append(values, query)
	}
	return len([]byte(strings.Join(values, ""))) + len([]byte(activity.URL))
}

func (c *Conversation) refreshMCPRuntime(runtime Runtime, executor *tool.Executor, baseTools []tool.Tool) (Runtime, error) {
	state := MCPRuntimeState{}
	if runtime.MCPState != nil {
		state = filterMCPRuntimeState(runtime.MCPState(), runtime.PermissionMode)
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

type expandedWebCall struct {
	parentIndex int
	childIndex  int
	kind        protocol.ToolKind
	value       string
	callID      string
}

type toolCallExpansion struct {
	calls          []protocol.ToolCall
	parentChildren [][]int
	web            map[string]expandedWebCall
}

func expandToolCalls(calls []protocol.ToolCall, plan webtool.ResolvedWebToolPlan) toolCallExpansion {
	expansion := toolCallExpansion{
		calls: make([]protocol.ToolCall, 0, len(calls)), parentChildren: make([][]int, len(calls)),
		web: make(map[string]expandedWebCall),
	}
	local := make(map[string]webtool.ResolvedTool, len(plan.Local))
	for _, resolved := range plan.Local {
		local[resolved.Definition.Name] = resolved
	}
	for parentIndex, call := range calls {
		resolved, isWeb := local[call.Name]
		values, err := webtool.InputValues(resolved.Definition.Kind, call.Arguments)
		if !isWeb || err != nil {
			index := len(expansion.calls)
			expansion.calls = append(expansion.calls, call)
			expansion.parentChildren[parentIndex] = append(expansion.parentChildren[parentIndex], index)
			if isWeb {
				expansion.web[call.ID] = expandedWebCall{parentIndex: parentIndex, kind: resolved.Definition.Kind, callID: call.ID}
			}
			continue
		}
		field := "query"
		if resolved.Definition.Kind == protocol.ToolWebFetch {
			field = "url"
		}
		for childIndex, value := range values {
			childID := call.ID
			if len(values) > 1 {
				childID = fmt.Sprintf("%s:%d", call.ID, childIndex+1)
			}
			arguments, _ := json.Marshal(map[string]any{field: value, "_eylu_batch_id": call.ID})
			child := protocol.ToolCall{ID: childID, Name: call.Name, Arguments: arguments}
			index := len(expansion.calls)
			expansion.calls = append(expansion.calls, child)
			expansion.parentChildren[parentIndex] = append(expansion.parentChildren[parentIndex], index)
			expansion.web[childID] = expandedWebCall{
				parentIndex: parentIndex, childIndex: childIndex, kind: resolved.Definition.Kind, value: value, callID: childID,
			}
		}
	}
	return expansion
}

func collapseToolResults(calls []protocol.ToolCall, expansion toolCallExpansion, executed []protocol.ToolResult) ([]protocol.ToolResult, []protocol.Part) {
	results := make([]protocol.ToolResult, len(calls))
	webParts := make([]protocol.Part, 0, len(expansion.web)*2)
	for parentIndex, parent := range calls {
		children := expansion.parentChildren[parentIndex]
		if len(children) == 0 {
			results[parentIndex] = protocol.ToolResult{CallID: parent.ID, Content: "tool call was not scheduled", IsError: true}
			continue
		}
		for _, resultIndex := range children {
			if resultIndex >= len(executed) {
				continue
			}
			if info, ok := expansion.web[expansion.calls[resultIndex].ID]; ok {
				webParts = append(webParts, webPartsForResult(info, executed[resultIndex])...)
			}
		}
		if len(children) == 1 {
			result := executed[children[0]]
			result.CallID = parent.ID
			results[parentIndex] = result
			continue
		}
		results[parentIndex] = collapseWebBatch(parent, children, expansion, executed)
	}
	return results, webParts
}

func collapseWebBatch(parent protocol.ToolCall, children []int, expansion toolCallExpansion, executed []protocol.ToolResult) protocol.ToolResult {
	type batchItem struct {
		Query             string          `json:"query,omitempty"`
		URL               string          `json:"url,omitempty"`
		Content           string          `json:"content"`
		StructuredContent json.RawMessage `json:"structured_content,omitempty"`
		IsError           bool            `json:"is_error,omitempty"`
		Truncated         bool            `json:"truncated,omitempty"`
	}
	items := make([]batchItem, 0, len(children))
	activities := make([]protocol.WebActivity, 0, len(children))
	citations := make([]protocol.URLCitation, 0)
	metadata := map[string]any{"web_status": string(protocol.WebStatusCompleted), "web_query_count": len(children), "untrusted_web_content": true}
	failed := 0
	truncated := false
	var content strings.Builder
	for position, resultIndex := range children {
		result := executed[resultIndex]
		info := expansion.web[expansion.calls[resultIndex].ID]
		item := batchItem{Content: result.Content, StructuredContent: result.StructuredContent, IsError: result.IsError, Truncated: result.Truncated}
		if info.kind == protocol.ToolWebFetch {
			item.URL = info.value
		} else {
			item.Query = info.value
		}
		items = append(items, item)
		if result.IsError {
			failed++
		}
		truncated = truncated || result.Truncated
		if position > 0 {
			content.WriteString("\n\n")
		}
		fmt.Fprintf(&content, "[%d] %s\n%s", position+1, info.value, result.Content)
		for _, part := range webPartsForResult(info, result) {
			if part.WebActivity != nil {
				activities = append(activities, *part.WebActivity)
			}
			if part.Citation != nil {
				citations = append(citations, *part.Citation)
			}
		}
		mergeWebResultMetadata(metadata, result.Metadata)
	}
	metadata["web_failed_count"] = failed
	metadata["activity_count"] = len(activities)
	metadata["citation_count"] = len(citations)
	if failed == len(children) {
		metadata["web_status"] = string(protocol.WebStatusError)
	}
	structured, _ := json.Marshal(map[string]any{"results": items, "activities": activities, "citations": citations})
	return protocol.ToolResult{
		CallID: parent.ID, Content: content.String(), StructuredContent: structured,
		IsError: failed == len(children), Truncated: truncated, Metadata: metadata,
	}
}

func mergeWebResultMetadata(target, source map[string]any) {
	for _, key := range []string{"web_backend", "web_kind", "web_target"} {
		if target[key] == nil && source[key] != nil {
			target[key] = source[key]
		}
	}
	for _, key := range []string{"web_input_tokens", "web_output_tokens"} {
		if value, ok := source[key].(int); ok {
			current, _ := target[key].(int)
			target[key] = current + value
		}
	}
	if value, ok := source["web_cost_usd"].(float64); ok {
		current, _ := target["web_cost_usd"].(float64)
		target["web_cost_usd"] = current + value
	}
}

func webActivityForCall(call protocol.ToolCall, info expandedWebCall) protocol.WebActivity {
	activity := protocol.WebActivity{CallID: call.ID, Kind: info.kind, Status: protocol.WebStatusRunning}
	if info.kind == protocol.ToolWebFetch {
		activity.Action, activity.URL = "fetch", info.value
	} else {
		activity.Action, activity.Query = "search", info.value
	}
	return activity
}

func webPartsForResult(info expandedWebCall, result protocol.ToolResult) []protocol.Part {
	var payload struct {
		Activities []protocol.WebActivity `json:"activities"`
		Citations  []protocol.URLCitation `json:"citations"`
	}
	if len(result.StructuredContent) > 0 {
		_ = json.Unmarshal(result.StructuredContent, &payload)
	}
	if len(payload.Activities) == 0 {
		activity := webActivityForCall(protocol.ToolCall{ID: info.callID}, info)
		activity.Status = protocol.WebStatusCompleted
		if result.IsError {
			activity.Status = protocol.WebStatusError
			activity.Error = result.Content
		}
		payload.Activities = []protocol.WebActivity{activity}
	}
	parts := make([]protocol.Part, 0, len(payload.Activities)+len(payload.Citations))
	callIDs := make(map[string]string, len(payload.Activities))
	for index, source := range payload.Activities {
		activity := source
		providerCallID := activity.CallID
		activity.CallID = info.callID
		if index > 0 {
			activity.CallID = fmt.Sprintf("%s:%d", info.callID, index+1)
		}
		if providerCallID != "" {
			callIDs[providerCallID] = activity.CallID
		}
		if activity.Kind == "" {
			activity.Kind = info.kind
		}
		if index == 0 {
			if activity.Kind == protocol.ToolWebFetch && activity.URL == "" {
				activity.URL = info.value
			}
			if activity.Kind == protocol.ToolWebSearch && activity.Query == "" {
				activity.Query = info.value
			}
		}
		if activity.Action == "" {
			if activity.Kind == protocol.ToolWebFetch {
				activity.Action = "fetch"
			} else {
				activity.Action = "search"
			}
		}
		activity.Queries = append([]string(nil), activity.Queries...)
		activity.Sources = append([]protocol.WebSource(nil), activity.Sources...)
		if result.IsError {
			activity.Status = protocol.WebStatusError
			activity.Error = result.Content
		} else if activity.Status == "" || activity.Status == protocol.WebStatusRunning {
			activity.Status = protocol.WebStatusCompleted
		}
		parts = append(parts, protocol.Part{Kind: protocol.PartWebActivity, WebActivity: &activity})
	}
	for _, source := range payload.Citations {
		citation := source
		if mapped := callIDs[citation.CallID]; mapped != "" {
			citation.CallID = mapped
		} else {
			citation.CallID = info.callID
		}
		parts = append(parts, protocol.Part{Kind: protocol.PartCitation, Citation: &citation})
	}
	return parts
}

func webStartedEvent(kind protocol.ToolKind) protocol.EventKind {
	if kind == protocol.ToolWebFetch {
		return protocol.EventWebFetchStarted
	}
	return protocol.EventWebSearchStarted
}

func webCompletedEvent(kind protocol.ToolKind) protocol.EventKind {
	if kind == protocol.ToolWebFetch {
		return protocol.EventWebFetchCompleted
	}
	return protocol.EventWebSearchCompleted
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
