package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	contextledger "Eylu/internal/context"
	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

type todoCaptureDriver struct {
	t        *testing.T
	requests int
}

func (d *todoCaptureDriver) Name() string { return "todo-capture" }
func (d *todoCaptureDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true}
}
func (d *todoCaptureDriver) Generate(_ context.Context, request driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.requests++
	if d.requests == 1 {
		call := protocol.ToolCall{ID: "todo-call", Name: "todolist", Arguments: json.RawMessage(`{"items":[{"id":"implement","content":"Implement task state","status":"in_progress"},{"id":"verify","content":"Verify behavior","status":"pending"}]}`)}
		return protocol.ModelResponse{Turn: protocol.Turn{ID: "todo-agent", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartToolCall, ToolCall: &call}}}, Stop: protocol.StopToolUse}, nil
	}
	text := requestText(request.Model.Turns)
	if !strings.Contains(text, "<task_list>") || !strings.Contains(text, "Implement task state") {
		d.t.Fatalf("task state missing from request: %s", text)
	}
	return protocol.ModelResponse{Turn: protocol.Turn{ID: "todo-final", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "done"}}}, Stop: protocol.StopCompleted}, nil
}

func TestConversationCapturesTodoListAndProtectsContext(t *testing.T) {
	model := &todoCaptureDriver{t: t}
	runtime := testRuntime(model, 1)
	executor := &tool.Executor{Registry: tool.NewRegistry(tool.NewTodoList()), Policy: policy.AllowAllChecker{}}
	events := make([]protocol.ModelEvent, 0)
	conversation := NewConversation()
	if _, err := conversation.Run(context.Background(), "implement", runtime, executor, LoopOptions{MaxTurns: 3}, false, func(event protocol.ModelEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	state := conversation.ExportState()
	if len(state.TodoList.Items) != 2 || state.TodoList.Items[0].ID != "implement" {
		t.Fatalf("state=%#v", state.TodoList)
	}
	foundEvent := false
	for _, event := range events {
		foundEvent = foundEvent || event.Kind == protocol.EventToolResult && event.ToolResult != nil && event.ToolResult.TodoList != nil
	}
	if !foundEvent {
		t.Fatalf("events=%#v", events)
	}
	foundCategory := false
	for _, category := range conversation.ContextReport().Categories {
		foundCategory = foundCategory || category.Category == contextledger.CategoryTaskState && category.Tokens > 0
	}
	if !foundCategory {
		t.Fatalf("context=%#v", conversation.ContextReport())
	}
	if summary := conversation.buildSummary(4096); !strings.Contains(summary, "[in_progress] Implement task state") || !strings.Contains(summary, "[pending] Verify behavior") {
		t.Fatalf("summary=%s", summary)
	}
}

func TestConversationTodoListRestoresClonesAndClearsOnNewSession(t *testing.T) {
	state := ConversationState{SessionID: "todo-state", PermissionMode: "manual", TodoList: protocol.TodoList{Items: []protocol.TodoItem{{ID: "one", Content: "First", Status: protocol.TodoPending}}}}
	conversation, err := RestoreConversation(state)
	if err != nil {
		t.Fatal(err)
	}
	state.TodoList.Items[0].Content = "mutated"
	got := conversation.TodoList()
	if got.Items[0].Content != "First" {
		t.Fatalf("todo list shared with input: %#v", got)
	}
	exported := conversation.ExportState()
	exported.TodoList.Items[0].Content = "changed"
	if conversation.TodoList().Items[0].Content != "First" {
		t.Fatal("exported todo list shared with conversation")
	}
	conversation.NewSession()
	if len(conversation.TodoList().Items) != 0 {
		t.Fatalf("new session todo list=%#v", conversation.TodoList())
	}
	_, err = RestoreConversation(ConversationState{SessionID: "invalid", TodoList: protocol.TodoList{Items: []protocol.TodoItem{{ID: "one", Content: "First", Status: "bad"}}}})
	if err == nil {
		t.Fatal("invalid persisted todo status was accepted")
	}
}
