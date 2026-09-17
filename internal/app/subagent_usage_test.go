package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/tool"
)

// multiRoundDriver makes the subagent take two model calls: the first asks for one
// tool call, the second finishes. Every call reports usage, so a runner that only
// reads the final response can be told apart from one that reads the whole run.
type multiRoundDriver struct {
	mu    sync.Mutex
	calls int
}

func (d *multiRoundDriver) Name() string { return "multi-round" }
func (d *multiRoundDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true, ParallelTools: true}
}

func (d *multiRoundDriver) Generate(_ context.Context, _ driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.mu.Lock()
	d.calls++
	call := d.calls
	d.mu.Unlock()
	if call == 1 {
		toolCall := protocol.ToolCall{ID: "call-1", Name: "subagent_echo", Arguments: json.RawMessage(`{}`)}
		return protocol.ModelResponse{
			Turn: protocol.Turn{ID: "agent-1", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartToolCall, ToolCall: &toolCall}}},
			Stop: protocol.StopToolUse, Usage: protocol.Usage{InputTokens: 100, OutputTokens: 10, Exact: true},
		}, nil
	}
	return protocol.ModelResponse{
		Turn: protocol.Turn{ID: "agent-2", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "done"}}},
		Stop: protocol.StopCompleted, Usage: protocol.Usage{InputTokens: 50, OutputTokens: 5, Exact: true},
	}, nil
}

// subagentEchoTool is a read-only tool the subagent profile allows.
type subagentEchoTool struct{ calls int }

func (t *subagentEchoTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: "subagent_echo", Description: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (t *subagentEchoTool) Risk() policy.Risk { return policy.RiskRead }
func (t *subagentEchoTool) Execute(context.Context, json.RawMessage) protocol.ToolResult {
	t.calls++
	return protocol.ToolResult{Content: "echoed"}
}

// A subagent's reported cost covers every model call its own request made.
//
// A subagent runs a whole request of its own, so reading the usage of the response
// it happened to finish on charges only the last call and loses every round before
// it - which for an agent that uses tools is most of what it spent.
func TestASubagentIsChargedForEveryModelCallItMade(t *testing.T) {
	workspace := t.TempDir()
	model := &multiRoundDriver{}
	parentExecutor := &tool.Executor{
		Registry: tool.NewRegistry(&subagentEchoTool{}), Policy: policy.AllowAllChecker{}, Workspace: workspace,
	}
	environment := searchTaskEnvironment{
		runtime: agent.Runtime{
			Provider: provider.Snapshot{Name: "subagent", Generation: 1, Config: config.ProviderConfig{
				Adapter: "multi-round", BaseURL: "https://example.test/v1", Model: "test-model", ContextWindow: 32_000,
			}},
			APIKey: "secret", Driver: model, Timeout: 5 * time.Second, Workspace: workspace, PermissionMode: "full",
		},
		config:      config.Default(),
		environment: conversationWithPrompts(t, "task-1").ExportState(),
		executor:    parentExecutor,
	}
	runner := &generalAgentRunner{environment: environment, taskID: "task-1", usageExact: true}
	result, err := runner.run(context.Background(), tool.AgentTaskRequest{
		SessionID: "session-1", SubagentType: "general", Prompt: "do the thing",
	}, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if result.Usage.InputTokens != 150 || result.Usage.OutputTokens != 15 {
		t.Fatalf("usage = %#v, want both calls (100+50 input, 10+5 output)", result.Usage)
	}
	if result.ModelCalls != 2 {
		t.Fatalf("model calls = %d, want 2", result.ModelCalls)
	}
	if !result.Usage.Exact {
		t.Fatalf("usage = %#v, want it reported as exact", result.Usage)
	}
	if result.Output != "done" {
		t.Fatalf("output = %q", result.Output)
	}
}

// The task snapshot carries the same figures as the result, so a reader of the
// task list sees the subagent's own cost rather than nothing.
func TestASubagentTaskReportsItsOwnCost(t *testing.T) {
	manager := tool.NewAgentTaskManager(1, nil, nil)
	runner := &fakeAgentRunner{result: tool.AgentTaskResult{
		Output: "done", Usage: protocol.Usage{InputTokens: 150, OutputTokens: 15, Exact: true}, ModelCalls: 2,
	}}
	task, err := manager.LaunchWithFactory(context.Background(), tool.AgentTaskRequest{
		SessionID: "session-1", SubagentType: "general", Prompt: "do it",
	}, func(string, tool.AgentTaskRequest) tool.AgentTaskRunner { return runner.run })
	if err != nil {
		t.Fatal(err)
	}
	final := waitForTerminalTask(t, manager, task.SessionID, task.ID)
	if final.Usage.InputTokens != 150 || final.ModelCalls != 2 {
		t.Fatalf("task = %#v", final)
	}
}

type fakeAgentRunner struct{ result tool.AgentTaskResult }

func (r *fakeAgentRunner) run(context.Context, tool.AgentTaskRequest, tool.AgentTaskEmitter) (tool.AgentTaskResult, error) {
	return r.result, nil
}

func waitForTerminalTask(t *testing.T, manager *tool.AgentTaskManager, sessionID, taskID string) tool.AgentTask {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := manager.Output(context.Background(), sessionID, taskID, false, 0)
		if err != nil {
			t.Fatal(err)
		}
		switch task.Status {
		case tool.AgentTaskCompleted, tool.AgentTaskFailed, tool.AgentTaskCancelled:
			return task
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the task never reached a terminal state")
	return tool.AgentTask{}
}

var _ = strings.TrimSpace
