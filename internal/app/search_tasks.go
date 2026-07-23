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
)

type searchTaskEnvironment struct {
	manager      *provider.Manager
	runtime      agent.Runtime
	config       config.Config
	parentPrompt string
	environment  agent.ConversationState
	codeContext  *tool.CodeContext
	coordinator  *tool.ResourceCoordinator
}

type boundSearchTaskService struct {
	manager *tool.AgentTaskManager
	runner  tool.AgentTaskRunner
}

func (s boundSearchTaskService) Launch(ctx context.Context, request tool.AgentTaskRequest) (tool.AgentTask, error) {
	return s.manager.LaunchWithRunner(ctx, request, s.runner)
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
		environment: conversation.ExportState(), codeContext: codeContext, coordinator: coordinator,
	}
	if observer != nil {
		r.searchTaskObservers[conversation.SessionID()] = observer
	}
	managerService := r.searchTasks
	r.searchTaskMu.Unlock()
	service := boundSearchTaskService{manager: managerService, runner: func(ctx context.Context, request tool.AgentTaskRequest) (tool.SearchReport, error) {
		return r.runSearchTask(ctx, request, environment)
	}}
	if r.session != nil {
		managerService.Restore(r.session.AgentTasks())
		r.session.SetAgentTaskSource(func(sessionID string) []tool.AgentTask {
			tasks := managerService.Snapshots(sessionID)
			completed := tasks[:0]
			for _, task := range tasks {
				switch task.Status {
				case tool.AgentTaskCompleted, tool.AgentTaskFailed, tool.AgentTaskCancelled:
					completed = append(completed, task)
				}
			}
			return completed
		})
	}

	mode := modelRuntime.PermissionMode
	profile := agent.ProfileForMode(mode)
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

func (r *runtime) runSearchTask(ctx context.Context, request tool.AgentTaskRequest, environment searchTaskEnvironment) (tool.SearchReport, error) {
	if environment.codeContext == nil || environment.coordinator == nil {
		return tool.SearchReport{}, errors.New("search task code context is unavailable")
	}

	timeout := time.Duration(environment.config.SearchAgent.TimeoutSeconds) * time.Second
	taskCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	modelRuntime, err := r.resolveSearchAgentRuntime(taskCtx, environment, request.Prompt)
	if err != nil {
		return tool.SearchReport{}, err
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
	child := agent.NewConversationForProfile(profile, environment.environment.Environment)
	prompt := "Delegated repository search:\n" + request.Prompt
	if parent := strings.TrimSpace(environment.parentPrompt); parent != "" {
		prompt += "\n\nCurrent parent request:\n" + parent
	}
	response, err := child.Run(taskCtx, prompt, modelRuntime, executor, agent.LoopOptions{
		MaxTurns: profile.MaxTurns, MaxTotalTokens: environment.config.MaxTotalTokens,
	}, false, nil)
	if err != nil {
		return tool.SearchReport{}, err
	}
	return parseSearchReport(response)
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
	var content strings.Builder
	for _, part := range response.Turn.Parts {
		if part.Kind == protocol.PartText {
			content.WriteString(part.Text)
		}
	}
	raw := strings.TrimSpace(content.String())
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

func (r *runtime) promptWithCompletedSearchTasks(sessionID, prompt string) string {
	r.searchTaskMu.RLock()
	manager := r.searchTasks
	r.searchTaskMu.RUnlock()
	if manager == nil {
		return prompt
	}
	tasks := manager.PendingReports(sessionID)
	if len(tasks) == 0 {
		return prompt
	}
	payload, err := json.Marshal(tasks)
	if err != nil {
		return prompt
	}
	return prompt + "\n\n<completed_search_tasks>\n" + string(payload) + "\n</completed_search_tasks>"
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
