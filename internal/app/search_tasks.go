package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/tool"
	"Eylu/internal/webtool"
)

type searchTaskEnvironment struct {
	manager      *provider.Manager
	runtime      agent.Runtime
	config       config.Config
	parentPrompt string
	environment  agent.ConversationState
	codeContext  *tool.CodeContext
	coordinator  *tool.ResourceCoordinator
	executor     *tool.Executor
}

type boundSearchTaskService struct {
	manager *tool.AgentTaskManager
	factory tool.AgentTaskRunnerFactory
}

func (s boundSearchTaskService) Launch(ctx context.Context, request tool.AgentTaskRequest) (tool.AgentTask, error) {
	return s.manager.LaunchWithFactory(ctx, request, s.factory)
}

func (s boundSearchTaskService) Output(ctx context.Context, sessionID, taskID string, block bool, timeout time.Duration) (tool.AgentTask, error) {
	return s.manager.Output(ctx, sessionID, taskID, block, timeout)
}

func (s boundSearchTaskService) Stop(sessionID, taskID string) (tool.AgentTask, error) {
	return s.manager.Stop(sessionID, taskID)
}

func (r *runtime) configureSearchAgent(manager *provider.Manager, conversation *agent.Conversation, modelRuntime agent.Runtime, executor *tool.Executor, cfg config.Config, prompt string, observer func(protocol.AgentTaskActivity)) error {
	r.searchTaskMu.Lock()
	if r.searchTasks == nil {
		r.searchTasks = tool.NewAgentTaskManager(cfg.MaxParallelAgents, nil, r.observeSearchTask)
	}
	if r.searchTaskObservers == nil {
		r.searchTaskObservers = make(map[string]func(protocol.AgentTaskActivity))
	}
	r.toolRuntimeMu.Lock()
	codeContext, coordinator := r.codeContext, r.resourceCoordinator
	r.toolRuntimeMu.Unlock()
	environment := searchTaskEnvironment{
		manager: manager, runtime: modelRuntime, config: cfg, parentPrompt: prompt,
		environment: conversation.ExportState(), codeContext: codeContext, coordinator: coordinator, executor: executor,
	}
	if observer != nil {
		r.searchTaskObservers[conversation.SessionID()] = observer
	}
	managerService := r.searchTasks
	r.searchTaskMu.Unlock()

	factory := func(taskID string, request tool.AgentTaskRequest) tool.AgentTaskRunner {
		switch request.SubagentType {
		case "search":
			runner := &searchAgentRunner{runtime: r, environment: environment, taskID: taskID, usageExact: true}
			return runner.run
		case "general":
			runner := &generalAgentRunner{environment: environment, manager: managerService, taskID: taskID, usageExact: true}
			return runner.run
		default:
			return nil
		}
	}
	service := boundSearchTaskService{manager: managerService, factory: factory}
	if r.session != nil {
		managerService.Restore(r.session.AgentTasks())
		r.session.SetAgentTaskSource(func(sessionID string) []tool.AgentTask {
			tasks := managerService.Snapshots(sessionID)
			terminal := tasks[:0]
			for _, task := range tasks {
				switch task.Status {
				case tool.AgentTaskCompleted, tool.AgentTaskFailed, tool.AgentTaskCancelled:
					terminal = append(terminal, task)
				}
			}
			return terminal
		})
	}

	profile := agent.ProfileForMode(modelRuntime.PermissionMode)
	for _, item := range []tool.Tool{
		tool.NewAgentTool(service, conversation.SessionID()), tool.NewTaskOutputTool(service, conversation.SessionID()), tool.NewTaskStopTool(service, conversation.SessionID()),
	} {
		definition := item.Definition()
		if !profile.AllowsTool(definition.Name, item.Risk()) {
			continue
		}
		if err := executor.Registry.Register(item); err != nil {
			return fmt.Errorf("register %s: %w", definition.Name, err)
		}
	}
	return nil
}

func (r *runtime) agentTaskManager(maxParallel int) *tool.AgentTaskManager {
	r.searchTaskMu.Lock()
	defer r.searchTaskMu.Unlock()
	if r.searchTasks == nil {
		r.searchTasks = tool.NewAgentTaskManager(maxParallel, nil, r.observeSearchTask)
		if r.session != nil {
			r.searchTasks.Restore(r.session.AgentTasks())
		}
	}
	return r.searchTasks
}

type searchAgentRunner struct {
	runtime     *runtime
	environment searchTaskEnvironment
	taskID      string
	child       *agent.Conversation
	started     bool
	usage       protocol.Usage
	usageExact  bool
}

func (r *searchAgentRunner) run(ctx context.Context, request tool.AgentTaskRequest, emit tool.AgentTaskEmitter) (tool.AgentTaskResult, error) {
	environment := r.environment
	if environment.codeContext == nil || environment.coordinator == nil {
		return tool.AgentTaskResult{}, errors.New("search task code context is unavailable")
	}
	timeout := time.Duration(environment.config.SearchAgent.TimeoutSeconds) * time.Second
	taskCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	modelRuntime, err := r.runtime.resolveSearchAgentRuntime(taskCtx, environment, request.Prompt)
	if err != nil {
		return tool.AgentTaskResult{}, err
	}
	profile := agent.SearchProfile(environment.config.SearchAgent.MaxTurns)
	modelRuntime.PermissionMode = profile.PermissionMode
	modelRuntime.SkillCatalog = ""
	modelRuntime.MCPContexts = nil
	modelRuntime.MCPToolServers = nil
	modelRuntime.MCPFingerprint = ""
	modelRuntime.MCPState = nil
	modelRuntime.WebDelegate = nil
	modelRuntime.ContextEvent = nil
	disabled := false
	modelRuntime.Provider.Config.WebTools = config.WebToolsConfig{
		Permission: config.WebPermissionDeny,
		Search:     config.WebToolConfig{Enabled: &disabled}, Fetch: config.WebToolConfig{Enabled: &disabled},
	}

	readFile := tool.NewReadFileWithContext(environment.codeContext, environment.config.MaxReadBytes)
	searchCode := tool.NewSearchCodeWithContext(environment.codeContext, environment.config.MaxSearchResults, int64(environment.config.MaxReadBytes))
	listDirectory := tool.NewListDirectoryWithContext(environment.codeContext, environment.config.MaxSearchResults*10)
	executor := &tool.Executor{
		Registry: tool.NewRegistry(readFile, searchCode, listDirectory), Policy: policy.AllowAllChecker{},
		Workspace: modelRuntime.Workspace, Timeout: time.Duration(environment.config.ToolTimeoutSec) * time.Second,
		MaxOutputBytes: environment.config.MaxOutputBytes, MaxParallelTools: environment.config.MaxParallelTools, Coordinator: environment.coordinator,
		SessionID: request.SessionID, ProviderName: modelRuntime.Provider.Name,
		ProviderGeneration: modelRuntime.Provider.Generation, Model: modelRuntime.Provider.Config.Model,
	}
	if r.child == nil {
		r.child = agent.NewConversationForProfile(profile, environment.environment.Environment)
	}
	prompt := request.Prompt
	if !r.started {
		prompt = "Delegated repository search:\n" + prompt
		if parent := strings.TrimSpace(environment.parentPrompt); parent != "" {
			prompt += "\n\nCurrent parent request:\n" + parent
		}
		r.started = true
	}
	response, err := r.child.Run(taskCtx, prompt, modelRuntime, executor, agent.LoopOptions{
		MaxTurns: profile.MaxTurns, MaxTotalTokens: environment.config.MaxTotalTokens,
	}, true, modelEventEmitter(emit))
	r.addUsage(response.Usage)
	result := tool.AgentTaskResult{Output: modelResponseText(response), Usage: r.usage, Transcript: r.child.Transcript()}
	if err != nil {
		return result, err
	}
	report, err := parseSearchReport(response)
	if err != nil {
		return result, err
	}
	result.Output, result.Report = report.Summary, &report
	return result, nil
}

func (r *searchAgentRunner) addUsage(usage protocol.Usage) {
	r.usage.InputTokens += usage.InputTokens
	r.usage.OutputTokens += usage.OutputTokens
	r.usage.ReasoningTokens += usage.ReasoningTokens
	r.usageExact = r.usageExact && usage.Exact
	r.usage.Exact = r.usageExact
}

type generalAgentRunner struct {
	environment searchTaskEnvironment
	manager     *tool.AgentTaskManager
	taskID      string
	child       *agent.Conversation
	executor    *tool.Executor
	started     bool
	usage       protocol.Usage
	usageExact  bool
}

func (r *generalAgentRunner) run(ctx context.Context, request tool.AgentTaskRequest, emit tool.AgentTaskEmitter) (tool.AgentTaskResult, error) {
	profile := agent.GeneralSubagentProfile(r.environment.runtime.PermissionMode, r.environment.config.MaxTurns)
	if r.child == nil {
		state := r.environment.environment
		state.SessionID = r.taskID
		state.DriverState = nil
		var err error
		r.child, err = agent.RestoreConversationForProfile(state, profile)
		if err != nil {
			return tool.AgentTaskResult{}, err
		}
		r.executor = generalAgentExecutor(r.environment.executor, profile, r.manager, r.taskID)
	}
	if r.executor == nil {
		return tool.AgentTaskResult{}, errors.New("general task executor is unavailable")
	}
	modelRuntime := r.environment.runtime
	modelRuntime.PermissionMode = profile.PermissionMode
	modelRuntime.ContextEvent = nil
	prompt := request.Prompt
	if !r.started {
		prompt = "Delegated task:\n" + prompt
		if parent := strings.TrimSpace(r.environment.parentPrompt); parent != "" {
			prompt += "\n\nCurrent parent request:\n" + parent
		}
		r.started = true
	}
	timeout := time.Duration(r.environment.config.MaxTurns) * modelRuntime.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	taskCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := r.child.Run(taskCtx, prompt, modelRuntime, r.executor, agent.LoopOptions{
		MaxTurns: profile.MaxTurns, MaxTotalTokens: r.environment.config.MaxTotalTokens, RequestID: r.taskID,
	}, true, modelEventEmitter(emit))
	r.addUsage(response.Usage)
	return tool.AgentTaskResult{Output: modelResponseText(response), Usage: r.usage, Transcript: r.child.Transcript()}, err
}

func (r *generalAgentRunner) addUsage(usage protocol.Usage) {
	r.usage.InputTokens += usage.InputTokens
	r.usage.OutputTokens += usage.OutputTokens
	r.usage.ReasoningTokens += usage.ReasoningTokens
	r.usageExact = r.usageExact && usage.Exact
	r.usage.Exact = r.usageExact
}

type agentTaskAuditSink struct {
	manager *tool.AgentTaskManager
	taskID  string
}

func (s *agentTaskAuditSink) Record(record tool.AuditRecord) {
	if s == nil || s.manager == nil {
		return
	}
	s.manager.EmitAuditEvent(s.taskID, record)
}

func generalAgentExecutor(parent *tool.Executor, profile agent.Profile, manager *tool.AgentTaskManager, taskID string) *tool.Executor {
	if parent == nil || parent.Registry == nil {
		return nil
	}
	registered := make([]tool.Tool, 0)
	for _, definition := range parent.Registry.Definitions() {
		item, ok := parent.Registry.Get(definition.Name)
		if !ok || !profile.AllowsTool(definition.Name, item.Risk()) {
			continue
		}
		if _, dynamicWebTool := item.(*webtool.LocalTool); dynamicWebTool {
			continue
		}
		if writeFile, ok := item.(*tool.WriteFile); ok {
			item = writeFile.CreateOnly()
		}
		registered = append(registered, item)
	}
	clone := *parent
	clone.Registry = tool.NewRegistry(registered...)
	clone.ProviderName = parent.ProviderName
	clone.Audit = &agentTaskAuditSink{manager: manager, taskID: taskID}
	if parent.Confirm != nil {
		confirm := parent.Confirm
		clone.Confirm = func(ctx context.Context, request policy.Request, outcome policy.Outcome) (tool.Confirmation, error) {
			return manager.Confirm(ctx, taskID, request, outcome, confirm)
		}
	}
	return &clone
}

func modelEventEmitter(emit tool.AgentTaskEmitter) func(protocol.ModelEvent) error {
	if emit == nil {
		return nil
	}
	return func(event protocol.ModelEvent) error {
		emit(event)
		return nil
	}
}

func modelResponseText(response protocol.ModelResponse) string {
	var content strings.Builder
	for _, part := range response.Turn.Parts {
		if part.Kind == protocol.PartText {
			content.WriteString(part.Text)
		}
	}
	return strings.TrimSpace(content.String())
}

func (r *runtime) resolveSearchAgentRuntime(ctx context.Context, environment searchTaskEnvironment, prompt string) (agent.Runtime, error) {
	searchConfig := environment.config.SearchAgent
	if strings.TrimSpace(searchConfig.Provider) == "" {
		runtime := environment.runtime
		if model := strings.TrimSpace(searchConfig.Model); model != "" {
			runtime.Provider.Config.Model = model
			runtime.Provider.Config.ReasoningEffort = ""
		}
		return runtime, nil
	}
	options := chatOptions{provider: searchConfig.Provider, model: searchConfig.Model}
	runtime, _, err := r.resolveRuntimeForPrompt(ctx, environment.manager, options, prompt, 0, 0, 0, false)
	if err != nil {
		return agent.Runtime{}, err
	}
	configureContextRuntime(&runtime, r.workspace, environment.config)
	return runtime, nil
}

func parseSearchReport(response protocol.ModelResponse) (tool.SearchReport, error) {
	raw := modelResponseText(response)
	if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSuffix(strings.TrimSpace(raw), "```")
	}
	start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if start < 0 || end < start {
		return tool.SearchReport{}, errors.New("search agent returned no JSON report")
	}
	var report tool.SearchReport
	if err := json.Unmarshal([]byte(raw[start:end+1]), &report); err != nil {
		return tool.SearchReport{}, fmt.Errorf("decode search agent report: %w", err)
	}
	if strings.TrimSpace(report.Summary) == "" {
		return tool.SearchReport{}, errors.New("search agent report summary is required")
	}
	for index, finding := range report.Findings {
		if strings.TrimSpace(finding.Path) == "" || finding.StartLine <= 0 || finding.EndLine < finding.StartLine || strings.TrimSpace(finding.Reason) == "" {
			return tool.SearchReport{}, fmt.Errorf("search agent finding %d is incomplete", index)
		}
		if finding.Confidence < 0 || finding.Confidence > 1 {
			return tool.SearchReport{}, fmt.Errorf("search agent finding %d confidence is outside 0..1", index)
		}
	}
	return report, nil
}

func (r *runtime) completedAgentNotifications(sessionID string) string {
	r.searchTaskMu.RLock()
	manager := r.searchTasks
	r.searchTaskMu.RUnlock()
	if manager == nil {
		return ""
	}
	tasks := manager.PendingNotifications(sessionID)
	if len(tasks) == 0 {
		return ""
	}
	type notification struct {
		TaskID       string               `json:"task_id"`
		SubagentType string               `json:"subagent_type"`
		Status       tool.AgentTaskStatus `json:"status"`
		Version      uint64               `json:"version"`
		Output       string               `json:"output,omitempty"`
		Report       *tool.SearchReport   `json:"report,omitempty"`
		Error        string               `json:"error,omitempty"`
	}
	items := make([]notification, 0, len(tasks))
	for _, task := range tasks {
		items = append(items, notification{
			TaskID: task.ID, SubagentType: task.SubagentType, Status: task.Status, Version: task.NotificationRevision,
			Output: task.Output, Report: task.Report, Error: task.Error,
		})
	}
	payload, err := json.Marshal(items)
	if err != nil {
		return ""
	}
	return "<agent_notification>\n" + string(payload) + "\n</agent_notification>"
}

func (r *runtime) promptWithCompletedSearchTasks(sessionID, prompt string) string {
	if notification := r.completedAgentNotifications(sessionID); notification != "" {
		return prompt + "\n\n" + notification
	}
	return prompt
}

func (r *runtime) observeSearchTask(task tool.AgentTask) {
	activity := protocol.AgentTaskActivity{
		TaskID: task.ID, SubagentType: task.SubagentType, Status: string(task.Status), Background: task.Background, Error: task.Error,
	}
	if task.Report != nil {
		activity.Report, _ = json.Marshal(task.Report)
	}
	r.searchTaskMu.RLock()
	observer := r.searchTaskObservers[task.SessionID]
	r.searchTaskMu.RUnlock()
	if observer != nil {
		observer(activity)
	}
}

func (r *runtime) closeSearchTasks() {
	r.searchTaskMu.Lock()
	manager := r.searchTasks
	r.searchTasks = nil
	r.searchTaskMu.Unlock()
	if manager != nil {
		manager.Close()
	}
}
