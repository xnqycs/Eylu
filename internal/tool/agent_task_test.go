package tool

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAgentTaskManagerForegroundAndBackgroundLifecycle(t *testing.T) {
	release := make(chan struct{})
	started := make(chan string, 3)
	var active atomic.Int32
	var maximum atomic.Int32
	manager := NewAgentTaskManager(2, func(ctx context.Context, request AgentTaskRequest) (SearchReport, error) {
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
			return SearchReport{Summary: request.Prompt, Findings: []SearchFinding{{Path: "main.go", StartLine: 1, EndLine: 1, Reason: "match", Confidence: 1}}}, nil
		case <-ctx.Done():
			return SearchReport{}, ctx.Err()
		}
	}, nil)
	defer manager.Close()

	for _, prompt := range []string{"one", "two", "three"} {
		task, err := manager.Launch(context.Background(), AgentTaskRequest{SessionID: "session", SubagentType: "search", Prompt: prompt, RunInBackground: true})
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

func TestAgentTaskBindsRunnerAtLaunch(t *testing.T) {
	manager := NewAgentTaskManager(1, func(context.Context, AgentTaskRequest) (SearchReport, error) {
		return SearchReport{Summary: "default"}, nil
	}, nil)
	defer manager.Close()
	task, err := manager.LaunchWithRunner(context.Background(), AgentTaskRequest{
		SessionID: "session", SubagentType: "search", Prompt: "find symbol",
	}, func(context.Context, AgentTaskRequest) (SearchReport, error) {
		return SearchReport{Summary: "bound"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != AgentTaskCompleted || task.Report == nil || task.Report.Summary != "bound" {
		t.Fatalf("task = %#v", task)
	}
}

func TestAgentTaskManagerForegroundStopAndRestore(t *testing.T) {
	manager := NewAgentTaskManager(1, func(ctx context.Context, _ AgentTaskRequest) (SearchReport, error) {
		<-ctx.Done()
		return SearchReport{}, ctx.Err()
	}, nil)
	task, err := manager.Launch(context.Background(), AgentTaskRequest{SessionID: "session", SubagentType: "search", Prompt: "wait", RunInBackground: true})
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

	restored := NewAgentTaskManager(1, func(context.Context, AgentTaskRequest) (SearchReport, error) {
		return SearchReport{}, nil
	}, nil)
	defer restored.Close()
	restored.Restore([]AgentTask{task, {ID: "running", Status: AgentTaskRunning}})
	snapshots := restored.Snapshots("session")
	if len(snapshots) != 1 || snapshots[0].Status != AgentTaskCancelled {
		t.Fatalf("restored = %#v", snapshots)
	}
}

func TestAgentTaskToolsValidateAndReturnStructuredStatus(t *testing.T) {
	manager := NewAgentTaskManager(1, func(context.Context, AgentTaskRequest) (SearchReport, error) {
		return SearchReport{Summary: "found"}, nil
	}, nil)
	defer manager.Close()
	agentTool := NewAgentTool(manager, "session")
	result := agentTool.Execute(context.Background(), json.RawMessage(`{"subagent_type":"search","prompt":"find entrypoint"}`))
	if result.IsError || !strings.Contains(result.Content, `"status": "completed"`) || len(result.StructuredContent) == 0 {
		t.Fatalf("agent result = %#v", result)
	}
	invalid := agentTool.Execute(context.Background(), json.RawMessage(`{"subagent_type":"other","prompt":"x"}`))
	if !invalid.IsError || !strings.Contains(invalid.Content, "unsupported") {
		t.Fatalf("invalid result = %#v", invalid)
	}
}
