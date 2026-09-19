package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/tool"
)

// recordingCheckpoint is a checkpoint sink that only remembers what it was told.
type recordingCheckpoint struct {
	mu          sync.Mutex
	intents     []tool.Intent
	completions []tool.Completion
}

func (s *recordingCheckpoint) RecordIntent(intent tool.Intent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intents = append(s.intents, intent)
	return nil
}

func (s *recordingCheckpoint) RecordCompletion(completion tool.Completion) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completions = append(s.completions, completion)
	return nil
}

// checkpointedParent builds a parent executor in auto mode with the given sink.
func checkpointedParent(t *testing.T, workspace string, sink tool.CheckpointSink, cfg config.Config) *tool.Executor {
	t.Helper()
	appRuntime := &runtime{workspace: workspace}
	parent, err := appRuntime.toolExecutorWith(cfg, chatOptions{mode: "auto"}, nil, nil, func(context.Context, policy.Request, policy.Outcome) (tool.Confirmation, error) {
		return tool.Confirmation{Approved: true}, nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent.Checkpoint = sink
	return parent
}

func subagentOf(t *testing.T, parent *tool.Executor, cfg config.Config) *tool.Executor {
	t.Helper()
	manager := tool.NewAgentTaskManager(1, nil, nil)
	t.Cleanup(manager.Close)
	child := generalAgentExecutor(parent, agent.GeneralSubagentProfile("auto", cfg.MaxTurns), manager, "task")
	if child == nil {
		t.Fatal("the subagent executor was not derived")
	}
	return child
}

// T-06: a subagent shares its parent's resource coordinator and checkpoint.
//
// Sharing is what keeps the parent's guarantees intact inside a delegated task:
// two agents cannot write the same path at once, and a side effect the subagent
// commits is recorded in the same trail the parent would have written it to.
func TestSubagentSharesTheParentCoordinatorAndCheckpoint(t *testing.T) {
	workspace := t.TempDir()
	sink := &recordingCheckpoint{}
	cfg := config.Default()
	parent := checkpointedParent(t, workspace, sink, cfg)
	child := subagentOf(t, parent, cfg)

	if child.Coordinator == nil || child.Coordinator != parent.Coordinator {
		t.Fatal("the subagent got its own resource coordinator, so parallel writes are no longer serialized against the parent")
	}
	if child.Checkpoint != tool.CheckpointSink(sink) {
		t.Fatal("the subagent got a different checkpoint, so its side effects leave the parent's trail")
	}
	if child.Workspace != parent.Workspace {
		t.Fatalf("the subagent workspace = %q, want the parent's %q", child.Workspace, parent.Workspace)
	}
	if child.Audit == nil || child.Audit == parent.Audit {
		t.Fatal("the subagent must report its own audit records, not the parent's sink")
	}

	// Sharing is observable, not just pointer equality: a write the subagent
	// commits reaches the parent's checkpoint.
	_, outcome := child.ExecuteBatchOutcome(context.Background(), "task", []protocol.ToolCall{
		{ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"path":"fresh.txt","content":"x","reason":"test"}`)},
	}, tool.BatchHooks{}, 1)
	if outcome.Control != protocol.ControlContinue && outcome.Control != protocol.ControlInterruptRequest {
		t.Fatalf("outcome = %#v", outcome)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.intents) != 1 || len(sink.completions) != 1 {
		t.Fatalf("the parent's checkpoint saw %d intents and %d completions, want one of each", len(sink.intents), len(sink.completions))
	}
	if sink.intents[0].Tool != "write_file" || sink.intents[0].TargetPath == "" {
		t.Fatalf("intent = %#v", sink.intents[0])
	}
}

// T-07: a subagent may only create files, never overwrite one.
//
// The parent keeps the full tool: the narrowing belongs to the delegated task,
// which cannot see the conversation that would justify replacing a file.
func TestSubagentWriteFileCannotOverwriteAnExistingFile(t *testing.T) {
	workspace := t.TempDir()
	existing := filepath.Join(workspace, "existing.txt")
	if err := os.WriteFile(existing, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	parent := checkpointedParent(t, workspace, nil, cfg)
	child := subagentOf(t, parent, cfg)

	childWrite, ok := child.Registry.Get("write_file")
	if !ok {
		t.Fatal("the subagent has no write_file at all")
	}
	result := childWrite.Execute(context.Background(), json.RawMessage(`{"path":"existing.txt","content":"changed","reason":"test"}`))
	if !result.IsError {
		t.Fatalf("the subagent overwrote an existing file: %#v", result)
	}
	data, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original" {
		t.Fatalf("the file changed to %q", data)
	}

	// A new path is still allowed, so the refusal is about overwriting rather
	// than about writing at all.
	created := childWrite.Execute(context.Background(), json.RawMessage(`{"path":"fresh.txt","content":"new","reason":"test"}`))
	if created.IsError {
		t.Fatalf("the subagent could not create a new file: %#v", created)
	}
	if data, err := os.ReadFile(filepath.Join(workspace, "fresh.txt")); err != nil || string(data) != "new" {
		t.Fatalf("fresh.txt = %q err = %v", data, err)
	}

	// The parent keeps the overwriting form.
	parentWrite, ok := parent.Registry.Get("write_file")
	if !ok {
		t.Fatal("the parent has no write_file")
	}
	if parentResult := parentWrite.Execute(context.Background(), json.RawMessage(`{"path":"existing.txt","content":"replaced","reason":"test"}`)); parentResult.IsError {
		t.Fatalf("the parent lost the ability to overwrite: %#v", parentResult)
	}
	if data, err := os.ReadFile(existing); err != nil || string(data) != "replaced" {
		t.Fatalf("existing.txt = %q err = %v", data, err)
	}
}

// T-08: a subagent's cost is its own, and the request that delegated to it is
// not charged for the rounds it ran.
func TestSubagentUsageIsNotFoldedIntoTheParentRequest(t *testing.T) {
	workspace := t.TempDir()
	model := &multiRoundDriver{}
	parent := conversationWithPrompts(t, "session-1", "the parent request")
	environment := searchTaskEnvironment{
		runtime: agent.Runtime{
			Provider: provider.Snapshot{Name: "subagent", Generation: 1, Config: config.ProviderConfig{
				Adapter: "multi-round", BaseURL: "https://example.test/v1", Model: "test-model", ContextWindow: 32_000,
			}},
			APIKey: "secret", Driver: model, Timeout: 5 * time.Second, Workspace: workspace, PermissionMode: "full",
		},
		config:      config.Default(),
		environment: parent.ExportState(),
		executor: &tool.Executor{
			Registry: tool.NewRegistry(&subagentEchoTool{}), Policy: policy.AllowAllChecker{}, Workspace: workspace,
		},
	}
	before := len(parent.ExportState().Turns)

	first := &generalAgentRunner{environment: environment, taskID: "task-1", usageExact: true}
	result, err := first.run(context.Background(), tool.AgentTaskRequest{
		SessionID: "session-1", SubagentType: "general", Prompt: "first",
	}, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if result.Usage.InputTokens != 150 || result.ModelCalls != 2 {
		t.Fatalf("the subagent reported %#v over %d calls, want its own two rounds", result.Usage, result.ModelCalls)
	}
	// The subagent's requests ran as its own session, on its own transcript. It
	// never shares the conversation of the request that delegated to it, which is
	// what keeps its cost and its turns out of that request.
	if first.child == nil {
		t.Fatal("the subagent ran without a conversation of its own")
	}
	childState := first.child.ExportState()
	if childState.SessionID != "task-1" || childState.SessionID == parent.SessionID() {
		t.Fatalf("the subagent ran in session %q, want its own", childState.SessionID)
	}
	if len(childState.Turns) == 0 {
		t.Fatal("the subagent produced no turns, so this fixture proves nothing")
	}
	if after := len(parent.ExportState().Turns); after != before {
		t.Fatalf("the parent request gained %d turn(s) from the subagent", after-before)
	}

	// A second task gets its own account rather than inheriting the first one's.
	// The fixture driver counts its rounds, so the second task needs its own.
	secondEnvironment := environment
	secondEnvironment.runtime.Driver = &multiRoundDriver{}
	second := &generalAgentRunner{environment: secondEnvironment, taskID: "task-2", usageExact: true}
	secondResult, err := second.run(context.Background(), tool.AgentTaskRequest{
		SessionID: "session-1", SubagentType: "general", Prompt: "second",
	}, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if secondResult.Usage.InputTokens != result.Usage.InputTokens || secondResult.ModelCalls != result.ModelCalls {
		t.Fatalf("the second task reported %#v over %d calls, want its own two rounds only", secondResult.Usage, secondResult.ModelCalls)
	}
	if !strings.Contains(secondResult.Output, "done") {
		t.Fatalf("second output = %q", secondResult.Output)
	}
}
