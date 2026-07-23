package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type AgentTaskStatus string

const (
	AgentTaskQueued          AgentTaskStatus = "queued"
	AgentTaskRunning         AgentTaskStatus = "running"
	AgentTaskWaitingApproval AgentTaskStatus = "waiting_approval"
	AgentTaskCompleted       AgentTaskStatus = "completed"
	AgentTaskFailed          AgentTaskStatus = "failed"
	AgentTaskCancelled       AgentTaskStatus = "cancelled"
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
	RunInBackground *bool  `json:"run_in_background,omitempty"` // Deprecated compatibility field; agents always run in the background.
}

func (AgentTaskRequest) Background() bool { return true }

type AgentTaskResult struct {
	Output     string          `json:"output,omitempty"`
	Report     *SearchReport   `json:"report,omitempty"`
	Usage      protocol.Usage  `json:"usage,omitzero"`
	Transcript []protocol.Turn `json:"transcript,omitempty"`
}

type AgentTaskConversationEntry struct {
	Prompt     string               `json:"prompt,omitempty"`
	ModelEvent *protocol.ModelEvent `json:"model_event,omitempty"`
	Audit      *AuditRecord         `json:"audit,omitempty"`
}

type AgentTask struct {
	ID                   string                       `json:"task_id"`
	SessionID            string                       `json:"session_id,omitempty"`
	SubagentType         string                       `json:"subagent_type"`
	Prompt               string                       `json:"prompt,omitempty"`
	Status               AgentTaskStatus              `json:"status"`
	Background           bool                         `json:"background"`
	CreatedAt            time.Time                    `json:"created_at"`
	UpdatedAt            time.Time                    `json:"updated_at,omitzero"`
	StartedAt            time.Time                    `json:"started_at,omitzero"`
	CompletedAt          time.Time                    `json:"completed_at,omitzero"`
	Output               string                       `json:"output,omitempty"`
	Report               *SearchReport                `json:"report,omitempty"`
	Usage                protocol.Usage               `json:"usage,omitzero"`
	Transcript           []protocol.Turn              `json:"transcript,omitempty"`
	Conversation         []AgentTaskConversationEntry `json:"conversation,omitempty"`
	ConversationRevision uint64                       `json:"conversation_revision,omitempty"`
	Error                string                       `json:"error,omitempty"`
	PendingMessages      int                          `json:"pending_messages,omitempty"`
	NotificationRevision uint64                       `json:"notification_revision,omitempty"`
	DeliveredRevision    uint64                       `json:"delivered_revision,omitempty"`
	Consumed             bool                         `json:"consumed,omitempty"` // Legacy session compatibility.
	ReadOnly             bool                         `json:"read_only,omitempty"`

	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	runner AgentTaskRunner
	queue  []AgentTaskRequest
}

type AgentTaskEmitter func(protocol.ModelEvent)
type AgentTaskRunner func(context.Context, AgentTaskRequest, AgentTaskEmitter) (AgentTaskResult, error)
type AgentTaskRunnerFactory func(string, AgentTaskRequest) AgentTaskRunner
type AgentTaskObserver func(AgentTask)

type AgentTaskEvent struct {
	Task       AgentTask            `json:"task"`
	ModelEvent *protocol.ModelEvent `json:"model_event,omitempty"`
	Audit      *AuditRecord         `json:"-"`
	Approval   *AgentApproval       `json:"-"`
}

type AgentApproval struct {
	TaskID   string
	Request  policy.Request
	Outcome  policy.Outcome
	Response chan Confirmation
}

type agentApprovalEnvelope struct {
	ctx      context.Context
	taskID   string
	request  policy.Request
	outcome  policy.Outcome
	fallback ConfirmFunc
	result   chan agentApprovalResult
}

type agentApprovalResult struct {
	confirmation Confirmation
	err          error
}

type agentTaskSubscriber struct {
	sessionID string
	events    chan AgentTaskEvent
	done      <-chan struct{}
}

type AgentTaskManager struct {
	ctx        context.Context
	cancel     context.CancelFunc
	runner     AgentTaskRunner
	observe    AgentTaskObserver
	limit      chan struct{}
	mu         sync.RWMutex
	tasks      map[string]*AgentTask
	subs       map[uint64]agentTaskSubscriber
	nextSub    uint64
	closed     bool
	wg         sync.WaitGroup
	approvals  chan agentApprovalEnvelope
	approvalWG sync.WaitGroup
}

func NewAgentTaskManager(maxParallel int, runner AgentTaskRunner, observer AgentTaskObserver) *AgentTaskManager {
	if maxParallel <= 0 {
		maxParallel = 2
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager := &AgentTaskManager{
		ctx: ctx, cancel: cancel, runner: runner, observe: observer, limit: make(chan struct{}, maxParallel),
		tasks: make(map[string]*AgentTask), subs: make(map[uint64]agentTaskSubscriber), approvals: make(chan agentApprovalEnvelope),
	}
	manager.approvalWG.Add(1)
	go manager.runApprovals()
	return manager
}

func (m *AgentTaskManager) Launch(ctx context.Context, request AgentTaskRequest) (AgentTask, error) {
	if m == nil {
		return AgentTask{}, errors.New("agent task service is unavailable")
	}
	return m.LaunchWithRunner(ctx, request, m.runner)
}

func (m *AgentTaskManager) LaunchWithRunner(ctx context.Context, request AgentTaskRequest, runner AgentTaskRunner) (AgentTask, error) {
	return m.LaunchWithFactory(ctx, request, func(string, AgentTaskRequest) AgentTaskRunner { return runner })
}

func (m *AgentTaskManager) LaunchWithFactory(_ context.Context, request AgentTaskRequest, factory AgentTaskRunnerFactory) (AgentTask, error) {
	if m == nil || factory == nil {
		return AgentTask{}, errors.New("agent task service is unavailable")
	}
	request.SubagentType = strings.ToLower(strings.TrimSpace(request.SubagentType))
	request.Prompt = strings.TrimSpace(request.Prompt)
	request.RunInBackground = nil
	if request.SubagentType != "search" && request.SubagentType != "general" {
		return AgentTask{}, fmt.Errorf("unsupported subagent_type %q", request.SubagentType)
	}
	if request.Prompt == "" {
		return AgentTask{}, errors.New("agent prompt is required")
	}
	now := time.Now().UTC()
	taskID := uuid.NewString()
	runner := factory(taskID, request)
	if runner == nil {
		return AgentTask{}, errors.New("agent task runner is unavailable")
	}
	taskCtx, cancel := context.WithCancel(m.ctx)
	task := &AgentTask{
		ID: taskID, SessionID: request.SessionID, SubagentType: request.SubagentType, Prompt: request.Prompt,
		Status: AgentTaskQueued, Background: request.Background(), CreatedAt: now, UpdatedAt: now,
		done: make(chan struct{}), ctx: taskCtx, cancel: cancel, runner: runner, queue: []AgentTaskRequest{request},
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		return AgentTask{}, errors.New("agent task service is closed")
	}
	m.tasks[task.ID] = task
	snapshot := cloneAgentTask(task)
	m.wg.Add(1)
	m.mu.Unlock()
	m.notify(snapshot)
	go m.run(task.ID)

	return snapshot, nil
}

func (m *AgentTaskManager) Continue(sessionID, taskID, prompt string) (AgentTask, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return AgentTask{}, errors.New("agent prompt is required")
	}
	m.mu.Lock()
	task := m.tasks[taskID]
	if task == nil || task.SessionID != sessionID {
		m.mu.Unlock()
		return AgentTask{}, fmt.Errorf("task %q was not found", taskID)
	}
	if task.ReadOnly || task.runner == nil {
		m.mu.Unlock()
		return AgentTask{}, fmt.Errorf("task %q was restored as read-only", taskID)
	}
	request := AgentTaskRequest{SessionID: sessionID, SubagentType: task.SubagentType, Prompt: prompt}
	startWorker := terminalAgentTaskStatus(task.Status)
	if startWorker {
		taskCtx, cancel := context.WithCancel(m.ctx)
		task.ctx, task.cancel = taskCtx, cancel
		task.done = make(chan struct{})
		task.Status = AgentTaskQueued
		task.CompletedAt = time.Time{}
		task.Error = ""
		task.Background = true
	}
	task.queue = append(task.queue, request)
	task.PendingMessages = len(task.queue)
	task.UpdatedAt = time.Now().UTC()
	snapshot := cloneAgentTask(task)
	if startWorker {
		m.wg.Add(1)
	}
	m.mu.Unlock()
	m.notify(snapshot)
	if startWorker {
		go m.run(taskID)
	}
	return snapshot, nil
}

func (m *AgentTaskManager) Output(ctx context.Context, sessionID, taskID string, block bool, timeout time.Duration) (AgentTask, error) {
	if !block {
		return m.snapshot(sessionID, taskID)
	}
	return m.wait(ctx, sessionID, taskID, timeout)
}

func (m *AgentTaskManager) Stop(sessionID, taskID string) (AgentTask, error) {
	m.mu.Lock()
	task := m.tasks[taskID]
	if task == nil || task.SessionID != sessionID {
		m.mu.Unlock()
		return AgentTask{}, fmt.Errorf("task %q was not found", taskID)
	}
	notify := false
	if activeAgentTaskStatus(task.Status) {
		task.cancel()
		task.queue = nil
		task.PendingMessages = 0
		task.Status = AgentTaskCancelled
		task.Error = context.Canceled.Error()
		task.CompletedAt = time.Now().UTC()
		task.UpdatedAt = task.CompletedAt
		task.NotificationRevision++
		if !task.Background {
			task.DeliveredRevision = task.NotificationRevision
		}
		closeTaskDone(task)
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
	return m.PendingNotifications(sessionID)
}

func (m *AgentTaskManager) HasPendingNotifications(sessionID string) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, task := range m.tasks {
		if task.SessionID == sessionID && task.Background && terminalAgentTaskStatus(task.Status) && task.DeliveredRevision < task.NotificationRevision {
			return true
		}
	}
	return false
}

func (m *AgentTaskManager) PendingNotifications(sessionID string) []AgentTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]AgentTask, 0)
	for _, task := range m.tasks {
		if task.SessionID != sessionID || !task.Background || !terminalAgentTaskStatus(task.Status) || task.DeliveredRevision >= task.NotificationRevision {
			continue
		}
		task.DeliveredRevision = task.NotificationRevision
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
		if task.NotificationRevision == 0 {
			task.NotificationRevision = 1
			if task.Consumed {
				task.DeliveredRevision = 1
			}
		}
		task.ReadOnly = true
		task.done = make(chan struct{})
		close(task.done)
		task.ctx = m.ctx
		task.cancel = func() {}
		task.runner = nil
		task.queue = nil
		task.PendingMessages = 0
		m.tasks[task.ID] = &task
	}
}

func (m *AgentTaskManager) Subscribe(ctx context.Context, sessionID string) <-chan AgentTaskEvent {
	events := make(chan AgentTaskEvent, 256)
	if m == nil {
		close(events)
		return events
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	m.nextSub++
	id := m.nextSub
	m.subs[id] = agentTaskSubscriber{sessionID: sessionID, events: events, done: ctx.Done()}
	m.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
		case <-m.ctx.Done():
		}
		m.mu.Lock()
		delete(m.subs, id)
		m.mu.Unlock()
	}()
	return events
}

func (m *AgentTaskManager) EmitModelEvent(taskID string, event protocol.ModelEvent) {
	if m == nil {
		return
	}
	m.mu.Lock()
	task := m.tasks[taskID]
	if task == nil {
		m.mu.Unlock()
		return
	}
	copyEvent := cloneAgentModelEvent(event)
	appendAgentConversationModelEvent(task, copyEvent)
	task.ConversationRevision++
	snapshot := cloneAgentTask(task)
	m.mu.Unlock()
	m.broadcast(AgentTaskEvent{Task: snapshot, ModelEvent: &copyEvent})
}

func (m *AgentTaskManager) EmitAuditEvent(taskID string, record AuditRecord) {
	if m == nil {
		return
	}
	m.mu.Lock()
	task := m.tasks[taskID]
	if task == nil {
		m.mu.Unlock()
		return
	}
	copyRecord := cloneAgentAuditRecord(record)
	task.Conversation = append(task.Conversation, AgentTaskConversationEntry{Audit: &copyRecord})
	task.ConversationRevision++
	snapshot := cloneAgentTask(task)
	m.mu.Unlock()
	m.broadcast(AgentTaskEvent{Task: snapshot, Audit: &copyRecord})
}

func (m *AgentTaskManager) MarkWaitingApproval(taskID string, waiting bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	task := m.tasks[taskID]
	if task == nil || terminalAgentTaskStatus(task.Status) {
		m.mu.Unlock()
		return
	}
	if waiting {
		task.Status = AgentTaskWaitingApproval
	} else {
		task.Status = AgentTaskRunning
	}
	task.UpdatedAt = time.Now().UTC()
	snapshot := cloneAgentTask(task)
	m.mu.Unlock()
	m.notify(snapshot)
}

func (m *AgentTaskManager) Confirm(ctx context.Context, taskID string, request policy.Request, outcome policy.Outcome, fallback ConfirmFunc) (Confirmation, error) {
	if m == nil {
		return Confirmation{}, errors.New("agent task service is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	result := make(chan agentApprovalResult, 1)
	envelope := agentApprovalEnvelope{ctx: ctx, taskID: taskID, request: request, outcome: outcome, fallback: fallback, result: result}
	select {
	case m.approvals <- envelope:
	case <-ctx.Done():
		return Confirmation{}, ctx.Err()
	case <-m.ctx.Done():
		return Confirmation{}, context.Canceled
	}
	select {
	case value := <-result:
		return value.confirmation, value.err
	case <-ctx.Done():
		return Confirmation{}, ctx.Err()
	case <-m.ctx.Done():
		return Confirmation{}, context.Canceled
	}
}

func (m *AgentTaskManager) runApprovals() {
	defer m.approvalWG.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case envelope := <-m.approvals:
			m.resolveApproval(envelope)
		}
	}
}

func (m *AgentTaskManager) resolveApproval(envelope agentApprovalEnvelope) {
	m.MarkWaitingApproval(envelope.taskID, true)
	defer m.MarkWaitingApproval(envelope.taskID, false)
	m.mu.RLock()
	task := m.tasks[envelope.taskID]
	hasSubscriber := false
	if task != nil {
		for _, subscriber := range m.subs {
			if subscriber.sessionID == "" || subscriber.sessionID == task.SessionID {
				hasSubscriber = true
				break
			}
		}
	}
	var snapshot AgentTask
	if task != nil {
		snapshot = cloneAgentTask(task)
	}
	m.mu.RUnlock()
	if task == nil {
		envelope.result <- agentApprovalResult{err: fmt.Errorf("task %q was not found", envelope.taskID)}
		return
	}
	if !hasSubscriber {
		if envelope.fallback == nil {
			envelope.result <- agentApprovalResult{err: errors.New("agent tool approval requires confirmation")}
			return
		}
		confirmation, err := envelope.fallback(envelope.ctx, envelope.request, envelope.outcome)
		envelope.result <- agentApprovalResult{confirmation: confirmation, err: err}
		return
	}
	response := make(chan Confirmation, 1)
	m.broadcast(AgentTaskEvent{Task: snapshot, Approval: &AgentApproval{
		TaskID: envelope.taskID, Request: envelope.request, Outcome: envelope.outcome, Response: response,
	}})
	select {
	case confirmation := <-response:
		envelope.result <- agentApprovalResult{confirmation: confirmation}
	case <-envelope.ctx.Done():
		envelope.result <- agentApprovalResult{err: envelope.ctx.Err()}
	case <-m.ctx.Done():
		envelope.result <- agentApprovalResult{err: context.Canceled}
	}
}

func (m *AgentTaskManager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.wg.Wait()
		return
	}
	m.closed = true
	m.cancel()
	notifications := make([]AgentTask, 0)
	for _, task := range m.tasks {
		if !activeAgentTaskStatus(task.Status) {
			continue
		}
		task.cancel()
		task.queue = nil
		task.PendingMessages = 0
		task.Status = AgentTaskCancelled
		task.Error = context.Canceled.Error()
		task.CompletedAt = time.Now().UTC()
		task.UpdatedAt = task.CompletedAt
		task.NotificationRevision++
		closeTaskDone(task)
		notifications = append(notifications, cloneAgentTask(task))
	}
	m.mu.Unlock()
	for _, task := range notifications {
		m.notify(task)
	}
	m.wg.Wait()
	m.approvalWG.Wait()
}

func (m *AgentTaskManager) run(taskID string) {
	defer m.wg.Done()
	for {
		m.mu.RLock()
		task := m.tasks[taskID]
		if task == nil || terminalAgentTaskStatus(task.Status) || len(task.queue) == 0 {
			m.mu.RUnlock()
			return
		}
		taskCtx := task.ctx
		m.mu.RUnlock()

		select {
		case m.limit <- struct{}{}:
		case <-taskCtx.Done():
			m.finishTurn(taskID, AgentTaskResult{}, taskCtx.Err())
			return
		}

		m.mu.Lock()
		task = m.tasks[taskID]
		if task == nil || terminalAgentTaskStatus(task.Status) || len(task.queue) == 0 {
			m.mu.Unlock()
			<-m.limit
			return
		}
		request := task.queue[0]
		task.queue = task.queue[1:]
		task.PendingMessages = len(task.queue)
		task.Conversation = append(task.Conversation, AgentTaskConversationEntry{Prompt: request.Prompt})
		task.ConversationRevision++
		task.Status = AgentTaskRunning
		task.UpdatedAt = time.Now().UTC()
		if task.StartedAt.IsZero() {
			task.StartedAt = task.UpdatedAt
		}
		runner := task.runner
		snapshot := cloneAgentTask(task)
		m.mu.Unlock()
		m.notify(snapshot)

		result, err := runner(taskCtx, request, func(event protocol.ModelEvent) { m.EmitModelEvent(taskID, event) })
		<-m.limit
		if m.finishTurn(taskID, result, err) {
			return
		}
	}
}

// finishTurn returns true when the task reached a terminal state.
func (m *AgentTaskManager) finishTurn(taskID string, result AgentTaskResult, err error) bool {
	m.mu.Lock()
	task := m.tasks[taskID]
	if task == nil || terminalAgentTaskStatus(task.Status) {
		m.mu.Unlock()
		return true
	}
	if result.Output != "" {
		task.Output = result.Output
	}
	if result.Report != nil {
		report := *result.Report
		report.Findings = append([]SearchFinding(nil), result.Report.Findings...)
		report.FollowUp = append([]string(nil), result.Report.FollowUp...)
		task.Report = &report
	}
	task.Usage = result.Usage
	if result.Transcript != nil {
		task.Transcript = cloneAgentTurns(result.Transcript)
	}
	task.UpdatedAt = time.Now().UTC()
	if err == nil && len(task.queue) > 0 {
		task.Status = AgentTaskQueued
		task.PendingMessages = len(task.queue)
		snapshot := cloneAgentTask(task)
		m.mu.Unlock()
		m.notify(snapshot)
		return false
	}
	task.CompletedAt = task.UpdatedAt
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		task.Status = AgentTaskCancelled
		task.Error = err.Error()
	case err != nil:
		task.Status = AgentTaskFailed
		task.Error = err.Error()
	default:
		task.Status = AgentTaskCompleted
		task.Error = ""
	}
	task.queue = nil
	task.PendingMessages = 0
	task.NotificationRevision++
	if !task.Background {
		task.DeliveredRevision = task.NotificationRevision
		task.Consumed = true
	}
	closeTaskDone(task)
	snapshot := cloneAgentTask(task)
	m.mu.Unlock()
	m.notify(snapshot)
	return true
}

func (m *AgentTaskManager) wait(ctx context.Context, sessionID, taskID string, timeout time.Duration) (AgentTask, error) {
	if ctx == nil {
		ctx = context.Background()
	}
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
		return m.snapshot(sessionID, taskID)
	case <-waitCtx.Done():
		snapshot, _ := m.snapshot(sessionID, taskID)
		return snapshot, waitCtx.Err()
	}
}

func (m *AgentTaskManager) snapshot(sessionID, taskID string) (AgentTask, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	task := m.tasks[taskID]
	if task == nil || task.SessionID != sessionID {
		return AgentTask{}, fmt.Errorf("task %q was not found", taskID)
	}
	return cloneAgentTask(task), nil
}

func (m *AgentTaskManager) notify(task AgentTask) {
	if m.observe != nil {
		m.observe(task)
	}
	m.broadcast(AgentTaskEvent{Task: task})
}

func (m *AgentTaskManager) broadcast(event AgentTaskEvent) {
	m.mu.RLock()
	subscribers := make([]agentTaskSubscriber, 0, len(m.subs))
	for _, subscriber := range m.subs {
		if subscriber.sessionID == "" || subscriber.sessionID == event.Task.SessionID {
			subscribers = append(subscribers, subscriber)
		}
	}
	m.mu.RUnlock()
	critical := event.Approval != nil || (event.ModelEvent == nil && event.Audit == nil && (terminalAgentTaskStatus(event.Task.Status) || event.Task.Status == AgentTaskWaitingApproval))
	for _, subscriber := range subscribers {
		if critical {
			select {
			case subscriber.events <- event:
			case <-subscriber.done:
			case <-m.ctx.Done():
			}
			continue
		}
		select {
		case subscriber.events <- event:
		default:
		}
	}
}

func cloneAgentTask(task *AgentTask) AgentTask {
	clone := *task
	clone.done, clone.ctx, clone.cancel, clone.runner, clone.queue = nil, nil, nil, nil, nil
	if task.Report != nil {
		report := *task.Report
		report.Findings = append([]SearchFinding(nil), task.Report.Findings...)
		report.FollowUp = append([]string(nil), task.Report.FollowUp...)
		clone.Report = &report
	}
	clone.Transcript = cloneAgentTurns(task.Transcript)
	clone.Conversation = cloneAgentConversation(task.Conversation)
	return clone
}

func appendAgentConversationModelEvent(task *AgentTask, event protocol.ModelEvent) {
	if task == nil {
		return
	}
	if event.Kind == protocol.EventTextDelta && event.Delta != "" && len(task.Conversation) > 0 {
		last := &task.Conversation[len(task.Conversation)-1]
		if last.ModelEvent != nil && last.ModelEvent.Kind == protocol.EventTextDelta {
			last.ModelEvent.Delta += event.Delta
			return
		}
	}
	task.Conversation = append(task.Conversation, AgentTaskConversationEntry{ModelEvent: &event})
}

func cloneAgentConversation(entries []AgentTaskConversationEntry) []AgentTaskConversationEntry {
	if entries == nil {
		return nil
	}
	payload, err := json.Marshal(entries)
	if err == nil {
		var cloned []AgentTaskConversationEntry
		if json.Unmarshal(payload, &cloned) == nil {
			return cloned
		}
	}
	return append([]AgentTaskConversationEntry(nil), entries...)
}

func cloneAgentModelEvent(event protocol.ModelEvent) protocol.ModelEvent {
	payload, err := json.Marshal(event)
	if err == nil {
		var cloned protocol.ModelEvent
		if json.Unmarshal(payload, &cloned) == nil {
			return cloned
		}
	}
	return event
}

func cloneAgentAuditRecord(record AuditRecord) AuditRecord {
	payload, err := json.Marshal(record)
	if err == nil {
		var cloned AuditRecord
		if json.Unmarshal(payload, &cloned) == nil {
			return cloned
		}
	}
	return record
}

func cloneAgentTurns(turns []protocol.Turn) []protocol.Turn {
	if turns == nil {
		return nil
	}
	payload, err := json.Marshal(turns)
	if err != nil {
		return append([]protocol.Turn(nil), turns...)
	}
	var cloned []protocol.Turn
	if json.Unmarshal(payload, &cloned) != nil {
		return append([]protocol.Turn(nil), turns...)
	}
	return cloned
}

func closeTaskDone(task *AgentTask) {
	if task.done == nil {
		return
	}
	select {
	case <-task.done:
	default:
		close(task.done)
	}
}

func activeAgentTaskStatus(status AgentTaskStatus) bool {
	return status == AgentTaskQueued || status == AgentTaskRunning || status == AgentTaskWaitingApproval
}

func terminalAgentTaskStatus(status AgentTaskStatus) bool {
	return status == AgentTaskCompleted || status == AgentTaskFailed || status == AgentTaskCancelled
}

func sortAgentTasks(tasks []AgentTask) {
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].CreatedAt.Before(tasks[j].CreatedAt) })
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
		Name: "agent", Description: "Delegate work to an isolated search or general subagent. Every task runs in the background and returns a task ID immediately. Use task_output to inspect the current state without waiting.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"subagent_type":{"type":"string","enum":["search","general"]},"prompt":{"type":"string","minLength":1}},"required":["subagent_type","prompt"],"additionalProperties":false}`),
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
		Name: "task_output", Description: "Inspect the current snapshot of a background subagent without waiting or consuming its completion notification.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"task_id":{"type":"string","minLength":1}},"required":["task_id"],"additionalProperties":false}`),
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
	task, err := t.service.Output(ctx, t.sessionID, input.TaskID, false, 0)
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
		Name: "task_stop", Description: "Cancel a queued, running, or approval-blocked subagent task and clear its pending messages.",
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
	view := task
	view.Transcript = nil
	payload, marshalErr := json.MarshalIndent(view, "", "  ")
	if marshalErr != nil {
		return toolError(marshalErr.Error())
	}
	return protocol.ToolResult{Content: string(payload), StructuredContent: payload, Metadata: map[string]any{"task_id": task.ID, "task_status": string(task.Status)}}
}
