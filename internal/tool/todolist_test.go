package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

func TestTodoListDefinitionAndValidReplacement(t *testing.T) {
	item := NewTodoList()
	definition := item.Definition()
	if definition.Name != "todolist" || !json.Valid(definition.InputSchema) || item.Risk() != policy.RiskSession {
		t.Fatalf("definition=%#v risk=%q", definition, item.Risk())
	}
	result := item.Execute(context.Background(), json.RawMessage(`{
		"explanation":"Start implementation",
		"items":[
			{"id":"inspect_repo","content":"Inspect the repository","status":"completed"},
			{"id":"implement_tools","content":"Implement the tools","status":"in_progress"},
			{"id":"run_tests","content":"Run tests","status":"pending"}
		]
	}`))
	if result.IsError || result.TodoList == nil || result.TodoList.Explanation != "Start implementation" {
		t.Fatalf("result=%#v", result)
	}
	if len(result.TodoList.Items) != 3 || result.TodoList.Items[1].Status != protocol.TodoInProgress {
		t.Fatalf("todo list=%#v", result.TodoList)
	}
	if !strings.Contains(result.Content, `"completed": 1`) || !strings.Contains(result.Content, `"remaining": 2`) {
		t.Fatalf("content=%s", result.Content)
	}
}

func TestSessionToolDefinitionsKeepTheirDeclaredSchemas(t *testing.T) {
	executor := &Executor{Registry: NewRegistry(NewTodoList(), NewAsk(func(context.Context, protocol.AskRequest) (protocol.AskResponse, error) {
		return protocol.AskResponse{}, nil
	}))}
	for _, definition := range executor.Definitions() {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		if _, injected := schema.Properties["reason"]; injected {
			t.Fatalf("%s schema contains injected reason: %s", definition.Name, definition.InputSchema)
		}
	}
}

func TestTodoListRejectsInvalidReplacementWithoutPayload(t *testing.T) {
	tooMany := make([]protocol.TodoItem, 21)
	for index := range tooMany {
		tooMany[index] = protocol.TodoItem{ID: "task_" + string(rune('a'+index)), Content: "task", Status: protocol.TodoPending}
	}
	encodedTooMany, _ := json.Marshal(map[string]any{"items": tooMany})
	tests := []json.RawMessage{
		json.RawMessage(`{"items":[{"id":"Bad ID","content":"x","status":"pending"}]}`),
		json.RawMessage(`{"items":[{"id":"same","content":"x","status":"pending"},{"id":"same","content":"y","status":"completed"}]}`),
		json.RawMessage(`{"items":[{"id":"one","content":"x","status":"in_progress"},{"id":"two","content":"y","status":"in_progress"}]}`),
		json.RawMessage(`{"items":[{"id":"one","content":"","status":"pending"}]}`),
		json.RawMessage(`{"items":[{"id":"one","content":"x","status":"unknown"}]}`),
		json.RawMessage(`{"items":[],"extra":true}`),
		encodedTooMany,
	}
	for _, input := range tests {
		result := NewTodoList().Execute(context.Background(), input)
		if !result.IsError || result.TodoList != nil {
			t.Fatalf("input=%s result=%#v", input, result)
		}
	}
	longContent, _ := json.Marshal(map[string]any{"items": []map[string]string{{"id": "long", "content": strings.Repeat("界", 201), "status": "pending"}}})
	if result := NewTodoList().Execute(context.Background(), longContent); !result.IsError {
		t.Fatalf("long content result=%#v", result)
	}
}

func TestTodoListEmptyReplacementClearsState(t *testing.T) {
	result := NewTodoList().Execute(context.Background(), json.RawMessage(`{"items":[]}`))
	if result.IsError || result.TodoList == nil || len(result.TodoList.Items) != 0 {
		t.Fatalf("result=%#v", result)
	}
}
