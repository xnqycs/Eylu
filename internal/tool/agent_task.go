package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type AgentTaskStatus string

const (
	AgentTaskQueued    AgentTaskStatus = "queued"
	AgentTaskRunning   AgentTaskStatus = "running"
	AgentTaskCompleted AgentTaskStatus = "completed"
	AgentTaskFailed    AgentTaskStatus = "failed"
	AgentTaskCancelled AgentTaskStatus = "cancelled"
)

type SearchFinding struct {
	Path       string  `json:"path"`
	StartLine  int     `json:"start_line"`
	EndLine    int     `json:"end_line"`
	Symbol     string  `json:"symbol,omitempty"`
	Reason     string  `json:"reason"`
	Confidence float64 `json:"confidence"`
	FileHash   string  `json:"file_hash,omitempty"`
}

type SearchReport struct {
	Summary  string          `json:"summary"`
	Findings []SearchFinding `json:"findings"`
	FollowUp []string        `json:"follow_up,omitempty"`
}

type AgentTaskRequest struct {
	SessionID       string `json:"session_id,omitempty"`
	SubagentType    string `json:"subagent_type"`
	Prompt          string `json:"prompt"`
	RunInBackground bool   `json:"run_in_background,omitempty"`
}

type AgentTask struct {
	ID           string          `json:"task_id"`
	SessionID    string          `json:"session_id,omitempty"`
	SubagentType string          `json:"subagent_type"`
	Prompt       string          `json:"prompt,omitempty"`
	Status       AgentTaskStatus `json:"status"`
	Background   bool            `json:"background"`
	CreatedAt    time.Time       `json:"created_at"`
	StartedAt    time.Time       `json:"started_at,omitzero"`
	CompletedAt  time.Time       `json:"completed_at,omitzero"`
	Report       *SearchReport   `json:"report,omitempty"`
	Error        string          `json:"error,omitempty"`
	Consumed     bool            `json:"consumed,omitempty"`

	done   chan struct{}
	cancel context.CancelFunc
}

type AgentTaskRunner func(context.Context, AgentTaskRequest) (SearchReport, error)
type AgentTaskObserver func(AgentTask)

type AgentTaskManager struct {
	ctx     context.Context
	cancel  context.CancelFunc
	runner  AgentTaskRunner
	observe AgentTaskObserver
	limit   chan struct{}
	mu      sync.RWMutex
	tasks   map[string]*AgentTask
	closed  bool
}

func NewAgentTaskManager(maxParallel int, runner AgentTaskRunner, observer AgentTaskObserver) *AgentTaskManager {
	if maxParallel <= 0 {
		maxParallel = 2
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &AgentTaskManager{ctx: ctx, cancel: cancel, runner: runner, observe: observer, limit: make(chan struct{}, maxParallel), tasks: make(map[string]*AgentTask)}
}

func (m *AgentTaskManager) Launch(ctx context.Context, request AgentTaskRequest) (AgentTask, error) {
	if m == nil {
		return AgentTask{}, errors.New("agent task service is unavailable")
	}
	return m.LaunchWithRunner(ctx, request, m.runner)
}

// LaunchWithRunner binds a task to the caller's immutable runtime snapshot
// while preserving the manager's shared concurrency limit and task store.
func (m *AgentTaskManager) LaunchWithRunner(ctx context.Context, request AgentTaskRequest, runner AgentTaskRunner) (AgentTask, error) {
	if m == nil || runner == nil {
		return AgentTask{}, errors.New("agent task service is unavailable")
	}
	request.SubagentType = strings.ToLower(strings.TrimSpace(request.SubagentType))
	request.Prompt = strings.TrimSpace(request.Prompt)
	if request.SubagentType != "search" {
		return AgentTask{}, fmt.Errorf("unsupported subagent_type %q", request.SubagentType)
	}
	if request.Prompt == "" {
		return AgentTask{}, errors.New("agent prompt is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return AgentTask{}, errors.New("agent task service is closed")
	}
	taskCtx := ctx
	if request.RunInBackground {
		taskCtx = m.ctx
	}
	taskCtx, cancel := context.WithCancel(taskCtx)
	task := &AgentTask{
		ID: uuid.NewString(), SessionID: request.SessionID, SubagentType: request.SubagentType,
		Prompt: request.Prompt, Status: AgentTaskQueued, Background: request.RunInBackground,
		CreatedAt: time.Now().UTC(), done: make(chan struct{}), cancel: cancel,
	}
	m.tasks[task.ID] = task
	snapshot := cloneAgentTask(task)
	m.mu.Unlock()
	m.notify(snapshot)
	go m.run(taskCtx, task.ID, request, runner)

	if request.RunInBackground {
		return snapshot, nil
	}
	completed, err := m.wait(ctx, request.SessionID, task.ID, 0, true)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return completed, err
}

func (m *AgentTaskManager) Output(ctx context.Context, sessionID, taskID string, block bool, timeout time.Duration) (AgentTask, error) {
	if !block {
		return m.snapshot(sessionID, taskID, true)
	}
	return m.wait(ctx, sessionID, taskID, timeout, true)
}

func (m *AgentTaskManager) Stop(sessionID, taskID string) (AgentTask, error) {
	m.mu.Lock()
	task := m.tasks[taskID]
	if task == nil || task.SessionID != sessionID {
		m.mu.Unlock()
		return AgentTask{}, fmt.Errorf("task %q was not found", taskID)
	}
	notify := false
	if task.Status == AgentTaskQueued || task.Status == AgentTaskRunning {
		task.cancel()
		task.Status = AgentTaskCancelled
		task.Error = context.Canceled.Error()
		task.CompletedAt = time.Now().UTC()
		close(task.done)
		notify = true
	}
	snapshot := cloneAgentTask(task)
	m.mu.Unlock()
	if notify {
		m.notify(snapshot)
	}
	return snapshot, nil
}

func (m *AgentTaskManager) PendingReports(sessionID string) []AgentTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]AgentTask, 0)
	for _, task := range m.tasks {
		if task.SessionID != sessionID || task.Consumed || !terminalAgentTaskStatus(task.Status) {
			continue
		}
		task.Consumed = true
		result = append(result, cloneAgentTask(task))
	}
	sortAgentTasks(result)
	return result
}

func (m *AgentTaskManager) Snapshots(sessionID string) []AgentTask {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]AgentTask, 0)
	for _, task := range m.tasks {
		if sessionID == "" || task.SessionID == sessionID {
			result = append(result, cloneAgentTask(task))
		}
	}
	sortAgentTasks(result)
	return result
}

func (m *AgentTaskManager) Restore(tasks []AgentTask) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, source := range tasks {
		if source.ID == "" || !terminalAgentTaskStatus(source.Status) {
			continue
		}
		if _, exists := m.tasks[source.ID]; exists {
			continue
		}
		task := source
		task.done = make(chan struct{})
		close(task.done)
		task.cancel = func() {}
		m.tasks[task.ID] = &task
	}
}

func (m *AgentTaskManager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.cancel()
	for _, task := range m.tasks {
		if task.Status == AgentTaskQueued || task.Status == AgentTaskRunning {
			task.cancel()
		}
	}
	m.mu.Unlock()
}

func (m *AgentTaskManager) run(ctx context.Context, taskID string, request AgentTaskRequest, runner AgentTaskRunner) {
	select {
	case m.limit <- struct{}{}:
		defer func() { <-m.limit }()
	case <-ctx.Done():
		m.finish(taskID, SearchReport{}, ctx.Err())
		return
	}
	m.mu.Lock()
	task := m.tasks[taskID]
	if task == nil || terminalAgentTaskStatus(task.Status) {
		m.mu.Unlock()
		return
	}
	task.Status = AgentTaskRunning
	task.StartedAt = time.Now().UTC()
	snapshot := cloneAgentTask(task)
	m.mu.Unlock()
	m.notify(snapshot)

	report, err := runner(ctx, request)
	m.finish(taskID, report, err)
}

func (m *AgentTaskManager) finish(taskID string, report SearchReport, err error) {
	m.mu.Lock()
	task := m.tasks[taskID]
	if task == nil || terminalAgentTaskStatus(task.Status) {
		m.mu.Unlock()
		return
	}
	task.CompletedAt = time.Now().UTC()
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		task.Status = AgentTaskCancelled
		task.Error = err.Error()
	case err != nil:
		task.Status = AgentTaskFailed
		task.Error = err.Error()
	default:
		task.Status = AgentTaskCompleted
		task.Report = &report
	}
	close(task.done)
	snapshot := cloneAgentTask(task)
	m.mu.Unlock()
	m.notify(snapshot)
}

func (m *AgentTaskManager) wait(ctx context.Context, sessionID, taskID string, timeout time.Duration, consume bool) (AgentTask, error) {
	m.mu.RLock()
	task := m.tasks[taskID]
	if task == nil || task.SessionID != sessionID {
		m.mu.RUnlock()
		return AgentTask{}, fmt.Errorf("task %q was not found", taskID)
	}
	done := task.done
	m.mu.RUnlock()
	waitCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	select {
	case <-done:
		return m.snapshot(sessionID, taskID, consume)
	case <-waitCtx.Done():
		return m.snapshot(sessionID, taskID, false)
	}
}

func (m *AgentTaskManager) snapshot(sessionID, taskID string, consume bool) (AgentTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task := m.tasks[taskID]
	if task == nil || task.SessionID != sessionID {
		return AgentTask{}, fmt.Errorf("task %q was not found", taskID)
	}
	if consume && terminalAgentTaskStatus(task.Status) {
		task.Consumed = true
	}
	return cloneAgentTask(task), nil
}

func (m *AgentTaskManager) notify(task AgentTask) {
	if m.observe != nil {
		m.observe(task)
	}
}

func cloneAgentTask(task *AgentTask) AgentTask {
	clone := *task
	clone.done, clone.cancel = nil, nil
	if task.Report != nil {
		report := *task.Report
		report.Findings = append([]SearchFinding(nil), task.Report.Findings...)
		report.FollowUp = append([]string(nil), task.Report.FollowUp...)
		clone.Report = &report
	}
	return clone
}

func terminalAgentTaskStatus(status AgentTaskStatus) bool {
	return status == AgentTaskCompleted || status == AgentTaskFailed || status == AgentTaskCancelled
}

func sortAgentTasks(tasks []AgentTask) {
	for i := 1; i < len(tasks); i++ {
		for j := i; j > 0 && tasks[j].CreatedAt.Before(tasks[j-1].CreatedAt); j-- {
			tasks[j], tasks[j-1] = tasks[j-1], tasks[j]
		}
	}
}

type AgentTaskService interface {
	Launch(context.Context, AgentTaskRequest) (AgentTask, error)
	Output(context.Context, string, string, bool, time.Duration) (AgentTask, error)
	Stop(string, string) (AgentTask, error)
}

type AgentTool struct {
	service   AgentTaskService
	sessionID string
}

func NewAgentTool(service AgentTaskService, sessionID string) *AgentTool {
	return &AgentTool{service: service, sessionID: sessionID}
}

func (t *AgentTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{
		Name: "agent", Description: "Delegate repository discovery to a read-only search subagent. Run foreground work when its report is needed immediately; background work returns a task ID.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"subagent_type":{"type":"string","enum":["search"]},"prompt":{"type":"string","minLength":1},"run_in_background":{"type":"boolean","default":false}},"required":["subagent_type","prompt"],"additionalProperties":false}`),
	}
}
func (t *AgentTool) Risk() policy.Risk        { return policy.RiskSession }
func (t *AgentTool) ParallelSafe() bool       { return true }
func (t *AgentTool) UseExecutorTimeout() bool { return false }
func (t *AgentTool) Execute(ctx context.Context, raw json.RawMessage) protocol.ToolResult {
	var input AgentTaskRequest
	if err := decodeStrict(raw, &input); err != nil {
		return toolError("invalid agent input: " + err.Error())
	}
	input.SessionID = t.sessionID
	task, err := t.service.Launch(ctx, input)
	return agentTaskResult(task, err)
}

type TaskOutputTool struct {
	service   AgentTaskService
	sessionID string
}

func NewTaskOutputTool(service AgentTaskService, sessionID string) *TaskOutputTool {
	return &TaskOutputTool{service: service, sessionID: sessionID}
}
func (t *TaskOutputTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{
		Name: "task_output", Description: "Inspect a search subagent task, optionally waiting for completion.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"task_id":{"type":"string","minLength":1},"block":{"type":"boolean","default":false},"timeout_ms":{"type":"integer","minimum":1,"maximum":300000,"default":30000}},"required":["task_id"],"additionalProperties":false}`),
	}
}
func (t *TaskOutputTool) Risk() policy.Risk        { return policy.RiskSession }
func (t *TaskOutputTool) ParallelSafe() bool       { return true }
func (t *TaskOutputTool) UseExecutorTimeout() bool { return false }
func (t *TaskOutputTool) Execute(ctx context.Context, raw json.RawMessage) protocol.ToolResult {
	var input struct {
		TaskID    string `json:"task_id"`
		Block     bool   `json:"block"`
		TimeoutMS int    `json:"timeout_ms"`
	}
	if err := decodeStrict(raw, &input); err != nil {
		return toolError("invalid task_output input: " + err.Error())
	}
	timeout := time.Duration(input.TimeoutMS) * time.Millisecond
	if input.Block && timeout <= 0 {
		timeout = 30 * time.Second
	}
	task, err := t.service.Output(ctx, t.sessionID, input.TaskID, input.Block, timeout)
	return agentTaskResult(task, err)
}

type TaskStopTool struct {
	service   AgentTaskService
	sessionID string
}

func NewTaskStopTool(service AgentTaskService, sessionID string) *TaskStopTool {
	return &TaskStopTool{service: service, sessionID: sessionID}
}
func (t *TaskStopTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{
		Name: "task_stop", Description: "Cancel a queued or running search subagent task.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"task_id":{"type":"string","minLength":1}},"required":["task_id"],"additionalProperties":false}`),
	}
}
func (t *TaskStopTool) Risk() policy.Risk  { return policy.RiskSession }
func (t *TaskStopTool) ParallelSafe() bool { return true }
func (t *TaskStopTool) Execute(_ context.Context, raw json.RawMessage) protocol.ToolResult {
	var input struct {
		TaskID string `json:"task_id"`
	}
	if err := decodeStrict(raw, &input); err != nil {
		return toolError("invalid task_stop input: " + err.Error())
	}
	task, err := t.service.Stop(t.sessionID, input.TaskID)
	return agentTaskResult(task, err)
}

func agentTaskResult(task AgentTask, err error) protocol.ToolResult {
	if err != nil {
		return toolError(err.Error())
	}
	payload, marshalErr := json.MarshalIndent(task, "", "  ")
	if marshalErr != nil {
		return toolError(marshalErr.Error())
	}
	return protocol.ToolResult{Content: string(payload), StructuredContent: payload, Metadata: map[string]any{"task_id": task.ID, "task_status": string(task.Status)}}
}
