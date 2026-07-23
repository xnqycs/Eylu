package tool

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type recordingAgentTaskService struct {
	block   bool
	timeout time.Duration
}

func (s *recordingAgentTaskService) Launch(context.Context, AgentTaskRequest) (AgentTask, error) {
	return AgentTask{}, nil
}

func (s *recordingAgentTaskService) Output(_ context.Context, sessionID, taskID string, block bool, timeout time.Duration) (AgentTask, error) {
	s.block, s.timeout = block, timeout
	return AgentTask{ID: taskID, SessionID: sessionID, Status: AgentTaskRunning, Background: true}, nil
}

func (s *recordingAgentTaskService) Stop(string, string) (AgentTask, error) {
	return AgentTask{}, nil
}

func TestAgentTaskManagerBackgroundLifecycle(t *testing.T) {
	release := make(chan struct{})
	started := make(chan string, 3)
	var active atomic.Int32
	var maximum atomic.Int32
	manager := NewAgentTaskManager(2, func(ctx context.Context, request AgentTaskRequest, _ AgentTaskEmitter) (AgentTaskResult, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- request.Prompt
		select {
		case <-release:
			report := SearchReport{Summary: request.Prompt, Findings: []SearchFinding{{Path: "main.go", StartLine: 1, EndLine: 1, Reason: "match", Confidence: 1}}}
			return AgentTaskResult{Output: request.Prompt, Report: &report}, nil
		case <-ctx.Done():
			return AgentTaskResult{}, ctx.Err()
		}
	}, nil)
	defer manager.Close()

	for _, prompt := range []string{"one", "two", "three"} {
		task, err := manager.Launch(context.Background(), AgentTaskRequest{SessionID: "session", SubagentType: "search", Prompt: prompt})
		if err != nil || task.Status != AgentTaskQueued {
			t.Fatalf("launch %s = %#v, %v", prompt, task, err)
		}
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("background task did not start")
		}
	}
	select {
	case prompt := <-started:
		t.Fatalf("parallel limit was exceeded by %q", prompt)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		tasks := manager.Snapshots("session")
		completed := 0
		for _, task := range tasks {
			if task.Status == AgentTaskCompleted {
				completed++
			}
		}
		if completed == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tasks did not complete: %#v", tasks)
		}
		time.Sleep(time.Millisecond)
	}
	if maximum.Load() != 2 || len(manager.PendingReports("session")) != 3 || len(manager.PendingReports("session")) != 0 {
		t.Fatalf("maximum=%d tasks=%#v", maximum.Load(), manager.Snapshots("session"))
	}
	persisted := manager.Snapshots("session")
	persisted[0].Consumed = false
	manager.Restore(persisted)
	if len(manager.PendingReports("session")) != 0 {
		t.Fatal("restore overwrote newer in-memory consumed state")
	}
	if _, err := manager.Output(context.Background(), "other-session", persisted[0].ID, false, 0); err == nil {
		t.Fatal("cross-session task access was accepted")
	}
}

func TestAgentTaskManagerBroadcastsAuditEvents(t *testing.T) {
	manager := NewAgentTaskManager(1, nil, nil)
	defer manager.Close()
	manager.Restore([]AgentTask{{ID: "task", SessionID: "session", Status: AgentTaskCompleted}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := manager.Subscribe(ctx, "session")

	manager.EmitAuditEvent("task", AuditRecord{CallID: "call", DurationMS: 42})
	select {
	case event := <-events:
		if event.Task.ID != "task" || event.Audit == nil || event.Audit.CallID != "call" || event.Audit.DurationMS != 42 {
			t.Fatalf("audit event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("audit event was not delivered")
	}
}

func TestAgentTaskManagerSnapshotsCompleteRunningConversation(t *testing.T) {
	started := make(chan struct{})
	manager := NewAgentTaskManager(1, func(ctx context.Context, _ AgentTaskRequest, emit AgentTaskEmitter) (AgentTaskResult, error) {
		emit(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: "earlier output"})
		emit(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: " continued"})
		call := protocol.ToolCall{ID: "call-history", Name: "read_file", Arguments: json.RawMessage(`{"path":"go.mod"}`)}
		emit(protocol.ModelEvent{Kind: protocol.EventToolStart, ToolCall: &call})
		result := protocol.ToolResult{CallID: call.ID, Content: "module Eylu"}
		emit(protocol.ModelEvent{Kind: protocol.EventToolResult, ToolResult: &result})
		close(started)
		<-ctx.Done()
		return AgentTaskResult{}, ctx.Err()
	}, nil)
	defer manager.Close()

	task, err := manager.Launch(context.Background(), AgentTaskRequest{
		SessionID: "session", SubagentType: "general", Prompt: "inspect go.mod",
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("agent history fixture did not start")
	}
	manager.EmitAuditEvent(task.ID, AuditRecord{CallID: "call-history", DurationMS: 17})

	snapshot, err := manager.Output(context.Background(), "session", task.ID, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Conversation) != 5 || snapshot.Conversation[0].Prompt != "inspect go.mod" {
		t.Fatalf("conversation = %#v", snapshot.Conversation)
	}
	if event := snapshot.Conversation[1].ModelEvent; event == nil || event.Kind != protocol.EventTextDelta || event.Delta != "earlier output continued" {
		t.Fatalf("text history = %#v", event)
	}
	if audit := snapshot.Conversation[4].Audit; audit == nil || audit.CallID != "call-history" || audit.DurationMS != 17 {
		t.Fatalf("audit history = %#v", audit)
	}
}

func TestAgentTaskOutputDoesNotConsumeNotification(t *testing.T) {
	manager := NewAgentTaskManager(1, func(context.Context, AgentTaskRequest, AgentTaskEmitter) (AgentTaskResult, error) {
		return AgentTaskResult{Output: "done"}, nil
	}, nil)
	defer manager.Close()
	task, err := manager.Launch(context.Background(), AgentTaskRequest{SessionID: "session", SubagentType: "general", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Output(context.Background(), "session", task.ID, true, time.Second); err != nil {
		t.Fatal(err)
	}
	if !manager.HasPendingNotifications("session") {
		t.Fatal("task output consumed the pending notification")
	}
	if notifications := manager.PendingNotifications("session"); len(notifications) != 1 || notifications[0].ID != task.ID {
		t.Fatalf("notifications = %#v", notifications)
	}
	if manager.HasPendingNotifications("session") {
		t.Fatal("delivered notification remained pending")
	}
}

func TestAgentTaskBindsRunnerAtLaunchAndForcesBackground(t *testing.T) {
	release := make(chan struct{})
	manager := NewAgentTaskManager(1, func(context.Context, AgentTaskRequest, AgentTaskEmitter) (AgentTaskResult, error) {
		return AgentTaskResult{Output: "default"}, nil
	}, nil)
	defer manager.Close()
	foreground := false
	task, err := manager.LaunchWithRunner(context.Background(), AgentTaskRequest{
		SessionID: "session", SubagentType: "search", Prompt: "find symbol",
		RunInBackground: &foreground,
	}, func(ctx context.Context, _ AgentTaskRequest, _ AgentTaskEmitter) (AgentTaskResult, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return AgentTaskResult{}, ctx.Err()
		}
		report := SearchReport{Summary: "bound"}
		return AgentTaskResult{Output: "bound", Report: &report}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != AgentTaskQueued || !task.Background {
		t.Fatalf("task = %#v", task)
	}
	close(release)
	completed, err := manager.Output(context.Background(), "session", task.ID, true, time.Second)
	if err != nil || completed.Status != AgentTaskCompleted || completed.Report == nil || completed.Report.Summary != "bound" {
		t.Fatalf("completed=%#v err=%v", completed, err)
	}
}

func TestAgentTaskManagerBackgroundStopAndRestore(t *testing.T) {
	manager := NewAgentTaskManager(1, func(ctx context.Context, _ AgentTaskRequest, _ AgentTaskEmitter) (AgentTaskResult, error) {
		<-ctx.Done()
		return AgentTaskResult{}, ctx.Err()
	}, nil)
	task, err := manager.Launch(context.Background(), AgentTaskRequest{SessionID: "session", SubagentType: "search", Prompt: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop("session", task.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		task, err = manager.Output(context.Background(), "session", task.ID, false, 0)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status == AgentTaskCancelled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task status = %s", task.Status)
		}
		time.Sleep(time.Millisecond)
	}
	manager.Close()

	restored := NewAgentTaskManager(1, func(context.Context, AgentTaskRequest, AgentTaskEmitter) (AgentTaskResult, error) {
		return AgentTaskResult{}, nil
	}, nil)
	defer restored.Close()
	restored.Restore([]AgentTask{task, {ID: "running", Status: AgentTaskRunning}})
	snapshots := restored.Snapshots("session")
	if len(snapshots) != 1 || snapshots[0].Status != AgentTaskCancelled || !snapshots[0].ReadOnly {
		t.Fatalf("restored = %#v", snapshots)
	}
	if _, err := restored.Continue("session", task.ID, "resume"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("restored continuation err = %v", err)
	}
}

func TestAgentTaskToolsValidateAndReturnStructuredStatus(t *testing.T) {
	manager := NewAgentTaskManager(1, func(context.Context, AgentTaskRequest, AgentTaskEmitter) (AgentTaskResult, error) {
		report := SearchReport{Summary: "found"}
		return AgentTaskResult{Output: "found", Report: &report}, nil
	}, nil)
	defer manager.Close()
	agentTool := NewAgentTool(manager, "session")
	definition := agentTool.Definition()
	if strings.Contains(string(definition.InputSchema), "run_in_background") || !strings.Contains(definition.Description, "Every task runs in the background") {
		t.Fatalf("agent definition = %#v", definition)
	}
	result := agentTool.Execute(context.Background(), json.RawMessage(`{"subagent_type":"search","prompt":"find entrypoint","run_in_background":false}`))
	if result.IsError || !strings.Contains(result.Content, `"status": "queued"`) || !strings.Contains(result.Content, `"background": true`) || len(result.StructuredContent) == 0 {
		t.Fatalf("agent result = %#v", result)
	}
	invalid := agentTool.Execute(context.Background(), json.RawMessage(`{"subagent_type":"other","prompt":"x"}`))
	if !invalid.IsError || !strings.Contains(invalid.Content, "unsupported") {
		t.Fatalf("invalid result = %#v", invalid)
	}
	outputService := &recordingAgentTaskService{}
	outputTool := NewTaskOutputTool(outputService, "session")
	outputDefinition := outputTool.Definition()
	if strings.Contains(string(outputDefinition.InputSchema), "block") || strings.Contains(string(outputDefinition.InputSchema), "timeout_ms") {
		t.Fatalf("task_output definition = %#v", outputDefinition)
	}
	output := outputTool.Execute(context.Background(), json.RawMessage(`{"task_id":"task","block":true,"timeout_ms":300000}`))
	if output.IsError || outputService.block || outputService.timeout != 0 {
		t.Fatalf("task_output=%#v block=%t timeout=%s", output, outputService.block, outputService.timeout)
	}
}

func TestAgentTaskManagerQueuesFollowUpMessagesSerially(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{}, 2)
	manager := NewAgentTaskManager(2, func(ctx context.Context, request AgentTaskRequest, _ AgentTaskEmitter) (AgentTaskResult, error) {
		started <- request.Prompt
		select {
		case <-release:
			return AgentTaskResult{Output: request.Prompt}, nil
		case <-ctx.Done():
			return AgentTaskResult{}, ctx.Err()
		}
	}, nil)
	defer manager.Close()

	task, err := manager.Launch(context.Background(), AgentTaskRequest{SessionID: "session", SubagentType: "general", Prompt: "first"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case prompt := <-started:
		if prompt != "first" {
			t.Fatalf("first prompt = %q", prompt)
		}
	case <-time.After(time.Second):
		t.Fatal("first turn did not start")
	}
	queued, err := manager.Continue("session", task.ID, "second")
	if err != nil {
		t.Fatal(err)
	}
	if queued.PendingMessages != 1 {
		t.Fatalf("pending messages = %d", queued.PendingMessages)
	}
	select {
	case prompt := <-started:
		t.Fatalf("follow-up started concurrently: %q", prompt)
	case <-time.After(25 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case prompt := <-started:
		if prompt != "second" {
			t.Fatalf("second prompt = %q", prompt)
		}
	case <-time.After(time.Second):
		t.Fatal("follow-up did not start")
	}
	release <- struct{}{}
	completed, err := manager.Output(context.Background(), "session", task.ID, true, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != AgentTaskCompleted || completed.Output != "second" {
		t.Fatalf("completed = %#v", completed)
	}
}

func TestAgentTaskApprovalsAreFIFOAndPauseOnlyRequestingTask(t *testing.T) {
	manager := NewAgentTaskManager(2, nil, nil)
	defer manager.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := manager.Subscribe(ctx, "session")
	first, err := manager.LaunchWithFactory(context.Background(), AgentTaskRequest{SessionID: "session", SubagentType: "general", Prompt: "first"}, func(taskID string, _ AgentTaskRequest) AgentTaskRunner {
		return func(ctx context.Context, _ AgentTaskRequest, _ AgentTaskEmitter) (AgentTaskResult, error) {
			confirmation, confirmErr := manager.Confirm(ctx, taskID, policy.Request{Tool: "write_file"}, policy.Outcome{Risk: policy.RiskWrite}, nil)
			if confirmErr != nil || !confirmation.Approved {
				return AgentTaskResult{}, confirmErr
			}
			return AgentTaskResult{Output: "first"}, nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.LaunchWithFactory(context.Background(), AgentTaskRequest{SessionID: "session", SubagentType: "general", Prompt: "second"}, func(taskID string, _ AgentTaskRequest) AgentTaskRunner {
		return func(ctx context.Context, _ AgentTaskRequest, _ AgentTaskEmitter) (AgentTaskResult, error) {
			confirmation, confirmErr := manager.Confirm(ctx, taskID, policy.Request{Tool: "bash"}, policy.Outcome{Risk: policy.RiskExec}, nil)
			if confirmErr != nil || !confirmation.Approved {
				return AgentTaskResult{}, confirmErr
			}
			return AgentTaskResult{Output: "second"}, nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	firstApproval := waitAgentApproval(t, events)
	quiet := time.After(25 * time.Millisecond)
waitForDecision:
	for {
		select {
		case event := <-events:
			if event.Approval != nil {
				t.Fatal("second approval arrived before the first decision")
			}
		case <-quiet:
			break waitForDecision
		}
	}
	firstApproval.Response <- Confirmation{Approved: true}
	secondApproval := waitAgentApproval(t, events)
	if secondApproval.TaskID == firstApproval.TaskID {
		t.Fatalf("approval task repeated: %s", secondApproval.TaskID)
	}
	secondApproval.Response <- Confirmation{Approved: true}
	for _, task := range []AgentTask{first, second} {
		completed, outputErr := manager.Output(context.Background(), "session", task.ID, true, time.Second)
		if outputErr != nil || completed.Status != AgentTaskCompleted {
			t.Fatalf("task=%#v err=%v", completed, outputErr)
		}
	}
}

func waitAgentApproval(t *testing.T, events <-chan AgentTaskEvent) *AgentApproval {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case event := <-events:
			if event.Approval != nil {
				return event.Approval
			}
		case <-deadline:
			t.Fatal("approval event was not delivered")
		}
	}
}
