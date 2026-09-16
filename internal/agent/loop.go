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
	// Usage, when non-nil, receives the accumulated usage of this run. The
	// response returned by Run keeps its own usage, which describes only the
	// final model call.
	Usage *RunUsage
}

var ErrRequestInterrupted = errors.New("request interrupted by user")

// StopNote describes a stop reason that must not be presented as a normal
// completion. It returns an empty string for a genuine completion.
func StopNote(stop protocol.StopKind) string {
	switch stop {
	case protocol.StopLength:
		return "the provider stopped the response before it finished, so the answer may be incomplete"
	case protocol.StopCancelled:
		return "the provider reported that the response was cancelled"
	default:
		return ""
	}
}

// Run performs one request.
//
// The conversation has a single writer: Run claims it for the whole request and a
// concurrent Run is refused with ErrConversationBusy. The state mutex is only
// held for short reads and commits, so the model call, the approval callback, the
// tool batch, the host event callbacks and every host-supplied function run with
// the state unlocked. A caller may therefore read ContextReport or ExportState
// from inside a callback, and during a blocked model call.
func (c *Conversation) Run(ctx context.Context, prompt string, runtime Runtime, executor *tool.Executor, options LoopOptions, stream bool, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	if err := c.gate.begin(); err != nil {
		return protocol.ModelResponse{}, err
	}
	defer c.gate.release()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.gate.attach(cancel)
	ctx = runCtx

	c.mu.Lock()
	if err := c.prepareRuntime(prompt, runtime); err != nil {
		c.mu.Unlock()
		return protocol.ModelResponse{}, err
	}
	if executor == nil {
		c.mu.Unlock()
		return protocol.ModelResponse{}, fmt.Errorf("tool executor is nil")
	}
	baseTools := registryToolsExcluding(executor.Registry, runtime.MCPToolServers)
	c.appendUser(prompt)
	c.mu.Unlock()

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
	plan, err := c.resolveWebRuntime(runtime, executor, webBudget)
	if err != nil {
		return protocol.ModelResponse{}, err
	}
	if err := authorizeHostedWeb(ctx, executor, runtime, plan, &hostedAuthorized); err != nil {
		return protocol.ModelResponse{}, err
	}
	c.applyToolDefinitions(runtime, plan.Definitions)
	definitions := plan.Definitions
	seenCalls := make(map[string]struct{})
	budget := newBudgetTracker(options.MaxTotalTokens)
	if options.Usage != nil {
		*options.Usage = RunUsage{}
	}
	requestID := options.RequestID
	if requestID == "" {
		requestID = uuid.NewString()
	}
	// reportUsage publishes the accumulated usage, so a caller never has to
	// mistake the final model call's usage for the whole request.
	reportUsage := func() {
		if options.Usage != nil {
			*options.Usage = budget.report()
		}
	}
	var last protocol.ModelResponse
	for iteration := 0; iteration < maxTurns; iteration++ {
		if err := ctx.Err(); err != nil {
			return protocol.ModelResponse{}, err
		}
		// A provider change that arrived during the previous round is applied here,
		// at the round boundary, instead of in the middle of a request.
		if snapshot, ok := c.takePendingSnapshot(); ok {
			c.mu.Lock()
			c.applyProviderSnapshotLocked(snapshot)
			c.mu.Unlock()
		}
		// Host callbacks run outside the state lock.
		if options.BeforeModel != nil {
			if message := strings.TrimSpace(options.BeforeModel()); message != "" {
				c.mu.Lock()
				c.appendUser(message)
				c.rebuildLedger(runtime)
				c.mu.Unlock()
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
		c.applyToolDefinitions(runtime, definitions)
		parallelToolCalls := executor.ParallelLimit() > 1 && driver.CapabilitiesFor(runtime.Driver, capabilityTarget(runtime)).ParallelTools
		response, effectiveRuntime, err := c.generate(ctx, runtime, definitions, parallelToolCalls, stream, emit, budget)
		if err != nil {
			// The last usable response is preserved, so a caller that stops on a
			// budget still sees the work that did happen.
			reportUsage()
			return last, err
		}
		// Validate before committing: a malformed response must not become part
		// of the history the next request is built from.
		response, err = normalizeModelResponse(response, seenCalls)
		if err != nil {
			reportUsage()
			c.mu.Lock()
			c.driverState = nil
			c.rebuildLedger(runtime)
			c.mu.Unlock()
			return last, err
		}
		c.mu.Lock()
		c.commitResponse(response, effectiveRuntime)
		c.mu.Unlock()
		last = response
		for _, call := range toolCalls(response.Turn) {
			seenCalls[call.ID] = struct{}{}
		}
		// Every committed call must end with a terminal state. closePending is
		// the single exit path for the calls of this response; it leaves a call
		// that already has a result untouched.
		calls := toolCalls(response.Turn)
		closePending := func(message string) {
			c.mu.Lock()
			c.closeToolCalls(runtime, calls, protocol.CallNotExecuted, message)
			c.mu.Unlock()
		}
		if err := recordHostedWebActivities(executor, runtime, requestID, response.Turn, plan, webBudget); err != nil {
			closePending("tool call was not executed: hosted web activity was rejected")
			reportUsage()
			return last, err
		}
		budget.add(callMain, response.Usage)
		if budget.exceeded() {
			// The request spent more than it was allowed to. The response that
			// caused it is kept and its calls are closed without executing, so the
			// history stays usable and no side effect runs on an exhausted budget.
			closePending("tool call was not executed: the request token budget was exhausted before execution")
			reportUsage()
			return last, budgetError(options.MaxTotalTokens, false)
		}
		// The stop reason decides what happens next. Only a genuine completion or a
		// tool-use request is a normal outcome; everything else is reported as
		// such instead of being presented as a completion.
		switch response.Stop {
		case protocol.StopCompleted:
			reportUsage()
			return response, nil
		case protocol.StopLength, protocol.StopCancelled:
			closePending("tool call was not executed: " + StopNote(response.Stop))
			reportUsage()
			return response, nil
		case protocol.StopError:
			closePending("tool call was not executed: the provider reported a failed response")
			reportUsage()
			return last, &protocol.Error{Code: protocol.ErrProtocol, Message: "model reported a failed response"}
		}
		if len(calls) == 0 {
			closePending("tool call was not executed: the model requested tool use without tool calls")
			reportUsage()
			return last, &protocol.Error{Code: protocol.ErrProtocol, Message: "model stopped for tool use without tool calls"}
		}
		toolTurn := protocol.Turn{ID: uuid.NewString(), Role: protocol.RoleTool, CreatedAt: time.Now().UTC()}
		runtime, err = c.refreshMCPRuntime(runtime, executor, baseTools)
		if err != nil {
			closePending("tool call was not executed: the tool registry could not be refreshed")
			reportUsage()
			return last, err
		}
		plan, err = c.resolveWebRuntime(runtime, executor, webBudget)
		if err != nil {
			closePending("tool call was not executed: web tools could not be resolved")
			reportUsage()
			return last, err
		}
		definitions = plan.Definitions
		c.applyToolDefinitions(runtime, definitions)
		expansion := expandToolCalls(calls, plan, newExecutionAllocator(callIDs(calls)))
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
							if part.WebActivity.CallID != activityCallID(info.executionID, 0) {
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
		executedResults, batchOutcome := executor.ExecuteBatchOutcome(ctx, requestID, expansion.calls, hooks, limit)
		results, webParts := collapseToolResults(calls, expansion, executedResults, batchOutcome)
		// The batch results are committed under the state lock, after every tool
		// and every callback has finished.
		c.mu.Lock()
		for index := range results {
			result := results[index]
			c.captureSkillResult(result)
			c.captureTodoListResult(result)
			toolTurn.Parts = append(toolTurn.Parts, protocol.Part{Kind: protocol.PartToolResult, ToolResult: &result})
		}
		toolTurn.Parts = append(toolTurn.Parts, webParts...)
		c.turns = append(c.turns, toolTurn)
		c.projectMapDirty = true
		if batchOutcome.Control != protocol.ControlContinue {
			// A remote driver state built from a response whose calls did not all
			// complete cannot be reused; the next request rebuilds it from the
			// local transcript.
			c.driverState = nil
		}
		c.rebuildLedger(runtime)
		c.mu.Unlock()
		// The request-level control state is owned by the executor. It is never
		// derived from tool content or from metadata rebuilt by Web aggregation,
		// so an external tool cannot interrupt or abort the host request.
		switch batchOutcome.Control {
		case protocol.ControlInterruptRequest:
			reportUsage()
			return last, ErrRequestInterrupted
		case protocol.ControlCancelRequest, protocol.ControlAbortRequest:
			reportUsage()
			return last, batchOutcome.Cause
		}
	}
	reportUsage()
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

// refreshMCPRuntime reads the live MCP catalog and applies it.
//
// The host callback that supplies the catalog runs outside the state lock, so it
// may read the conversation; the resulting state is applied under the lock.
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

type expandedWebCall struct {
	parentIndex int
	childIndex  int
	kind        protocol.ToolKind
	value       string
	// executionID is the host-owned identity of this execution.
	executionID string
	// parentCallID is the model call ID of the request that produced it.
	parentCallID string
}

type toolCallExpansion struct {
	calls          []protocol.ToolCall
	parentChildren [][]int
	web            map[string]expandedWebCall
}

// expandToolCalls turns one model response into the calls the executor runs.
//
// A call the host expands into several executions gets a host-owned execution ID;
// the model call ID is only kept as the explicit parent relation and as the ID the
// collapsed result is reported under. Calls the host does not expand are already
// provider-level calls, so their model ID is their execution identity.
func expandToolCalls(calls []protocol.ToolCall, plan webtool.ResolvedWebToolPlan, allocator *executionAllocator) toolCallExpansion {
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
				expansion.web[call.ID] = expandedWebCall{parentIndex: parentIndex, kind: resolved.Definition.Kind, executionID: call.ID, parentCallID: call.ID}
			}
			continue
		}
		field := "query"
		if resolved.Definition.Kind == protocol.ToolWebFetch {
			field = "url"
		}
		for childIndex, value := range values {
			executionID := allocator.allocate()
			arguments, _ := json.Marshal(map[string]any{field: value, "_eylu_batch_id": call.ID})
			child := protocol.ToolCall{ID: executionID, Name: call.Name, Arguments: arguments, ParentCallID: call.ID}
			index := len(expansion.calls)
			expansion.calls = append(expansion.calls, child)
			expansion.parentChildren[parentIndex] = append(expansion.parentChildren[parentIndex], index)
			expansion.web[executionID] = expandedWebCall{
				parentIndex: parentIndex, childIndex: childIndex, kind: resolved.Definition.Kind, value: value,
				executionID: executionID, parentCallID: call.ID,
			}
		}
	}
	return expansion
}

func collapseToolResults(calls []protocol.ToolCall, expansion toolCallExpansion, executed []protocol.ToolResult, outcome protocol.BatchOutcome) ([]protocol.ToolResult, []protocol.Part) {
	results := make([]protocol.ToolResult, len(calls))
	webParts := make([]protocol.Part, 0, len(expansion.web)*2)
	for parentIndex, parent := range calls {
		children := expansion.parentChildren[parentIndex]
		if len(children) == 0 {
			results[parentIndex] = protocol.ToolResult{CallID: parent.ID, Content: "tool call was not scheduled", IsError: true, State: protocol.CallNotExecuted}
			continue
		}
		for _, resultIndex := range children {
			if resultIndex >= len(executed) {
				continue
			}
			if info, ok := expansion.web[expansion.calls[resultIndex].ID]; ok && webActivityProjected(executed[resultIndex]) {
				webParts = append(webParts, webPartsForResult(info, executed[resultIndex])...)
			}
		}
		if len(children) == 1 {
			result := executed[children[0]]
			result.CallID = parent.ID
			results[parentIndex] = result
			continue
		}
		results[parentIndex] = collapseWebBatch(parent, children, expansion, executed, outcome)
	}
	return results, webParts
}

// collapseWebBatch aggregates an expanded Web fan-out for display.
//
// It only handles content, activity and per-child state. The request-level
// control state belongs to the executor, so it is deliberately neither derived
// nor dropped here; the transitional metadata merge exists solely for UI
// consumers that have not moved to the typed fields yet.
func collapseWebBatch(parent protocol.ToolCall, children []int, expansion toolCallExpansion, executed []protocol.ToolResult, outcome protocol.BatchOutcome) protocol.ToolResult {
	type batchItem struct {
		Query             string             `json:"query,omitempty"`
		URL               string             `json:"url,omitempty"`
		Content           string             `json:"content"`
		State             protocol.CallState `json:"call_state,omitempty"`
		StructuredContent json.RawMessage    `json:"structured_content,omitempty"`
		IsError           bool               `json:"is_error,omitempty"`
		Truncated         bool               `json:"truncated,omitempty"`
	}
	items := make([]batchItem, 0, len(children))
	activities := make([]protocol.WebActivity, 0, len(children))
	citations := make([]protocol.URLCitation, 0)
	states := make([]protocol.CallState, 0, len(children))
	metadata := map[string]any{"web_status": string(protocol.WebStatusCompleted), "web_query_count": len(children), "untrusted_web_content": true}
	failed := 0
	truncated := false
	var content strings.Builder
	for position, resultIndex := range children {
		result := executed[resultIndex]
		info := expansion.web[expansion.calls[resultIndex].ID]
		state := result.State
		if state == "" && resultIndex < len(outcome.States) {
			state = outcome.States[resultIndex]
		}
		states = append(states, state)
		item := batchItem{Content: result.Content, State: state, StructuredContent: result.StructuredContent, IsError: result.IsError, Truncated: result.Truncated}
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
		if webActivityProjected(result) {
			for _, part := range webPartsForResult(info, result) {
				if part.WebActivity != nil {
					activities = append(activities, *part.WebActivity)
				}
				if part.Citation != nil {
					citations = append(citations, *part.Citation)
				}
			}
		}
		mergeWebResultMetadata(metadata, result.Metadata)
		mergeLegacyControlMetadata(metadata, result.Metadata)
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
		IsError: failed == len(children), Truncated: truncated, State: aggregateCallState(states), Metadata: metadata,
	}
}

// aggregateCallState reduces the child states of an expanded batch to one state
// for the parent call. A mixed outcome never claims that every child succeeded.
func aggregateCallState(states []protocol.CallState) protocol.CallState {
	observed := make([]protocol.CallState, 0, len(states))
	for _, state := range states {
		if state != "" {
			observed = append(observed, state)
		}
	}
	if len(observed) == 0 {
		return protocol.CallNotExecuted
	}
	uniform := true
	for _, state := range observed[1:] {
		if state != observed[0] {
			uniform = false
			break
		}
	}
	if uniform {
		return observed[0]
	}
	for _, candidate := range []protocol.CallState{
		protocol.CallOutcomeUnknown, protocol.CallFailed, protocol.CallCancelled,
		protocol.CallRejected, protocol.CallNotExecuted,
	} {
		for _, state := range observed {
			if state == candidate {
				return candidate
			}
		}
	}
	return protocol.CallFailed
}

// webActivityProjected reports whether a child call produced an observable web
// activity. A call that never ran, or that was refused before execution, must
// not be projected as a completed search that failed.
func webActivityProjected(result protocol.ToolResult) bool {
	switch result.State {
	case protocol.CallNotExecuted, protocol.CallRejected:
		return false
	default:
		return true
	}
}

// mergeLegacyControlMetadata copies the transitional control metadata that UI
// consumers built before the typed states still read. New control logic must
// never read these keys: they are display-only and untrusted tool content can
// set them.
func mergeLegacyControlMetadata(target, source map[string]any) {
	for _, key := range []string{"interrupt_request", "approval_rejected", "rejection_reason", "batch_cancelled", "cancelled"} {
		if target[key] == nil && source[key] != nil {
			target[key] = source[key]
		}
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
		activity := webActivityForCall(protocol.ToolCall{ID: info.executionID}, info)
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
		// The activity identity is derived from the host-owned execution identity,
		// so it is stable across projections and can never collide with a model
		// call ID.
		activity.CallID = activityCallID(info.executionID, index)
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
			citation.CallID = activityCallID(info.executionID, 0)
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

// callIDs lists the model call IDs of one response. They are reserved for the
// execution allocator, so a host execution can never take an identity the model
// already used.
func callIDs(calls []protocol.ToolCall) []string {
	ids := make([]string, 0, len(calls))
	for _, call := range calls {
		ids = append(ids, call.ID)
	}
	return ids
}
