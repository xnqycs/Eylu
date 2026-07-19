package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

const (
	maxTodoItems        = 20
	maxTodoContentRunes = 200
)

var stableIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

type TodoList struct{}

func NewTodoList() *TodoList { return &TodoList{} }

func (*TodoList) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{
		Name:        "todolist",
		Description: "Replace the current session task list. Use this for non-trivial multi-step execution, update it immediately when work starts or finishes, and keep at most one item in_progress. An empty items array clears the list.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"explanation":{"type":"string","description":"Optional short explanation for this update"},"items":{"type":"array","maxItems":20,"items":{"type":"object","properties":{"id":{"type":"string","pattern":"^[a-z][a-z0-9_]*$"},"content":{"type":"string","minLength":1,"maxLength":200},"status":{"type":"string","enum":["pending","in_progress","completed","cancelled"]}},"required":["id","content","status"],"additionalProperties":false}}},"required":["items"],"additionalProperties":false}`),
	}
}

func (*TodoList) Risk() policy.Risk { return policy.RiskSession }

func (*TodoList) Execute(_ context.Context, raw json.RawMessage) protocol.ToolResult {
	var input protocol.TodoList
	if err := decodeStrict(raw, &input); err != nil {
		return toolError("invalid todolist input: " + err.Error())
	}
	input.Explanation = strings.TrimSpace(input.Explanation)
	if err := validateTodoItems(input.Items); err != nil {
		return toolError("invalid todolist input: " + err.Error())
	}
	input.Items = cloneTodoItems(input.Items)
	completed, remaining := todoProgress(input.Items)
	payload := struct {
		Explanation string              `json:"explanation,omitempty"`
		Items       []protocol.TodoItem `json:"items"`
		Completed   int                 `json:"completed"`
		Remaining   int                 `json:"remaining"`
	}{input.Explanation, input.Items, completed, remaining}
	encoded, _ := json.MarshalIndent(payload, "", "  ")
	result := input
	return protocol.ToolResult{Content: string(encoded), TodoList: &result}
}

func validateTodoItems(items []protocol.TodoItem) error {
	if len(items) > maxTodoItems {
		return fmt.Errorf("items exceeds maximum of %d", maxTodoItems)
	}
	seen := make(map[string]struct{}, len(items))
	inProgress := 0
	for index := range items {
		item := &items[index]
		item.ID = strings.TrimSpace(item.ID)
		item.Content = strings.TrimSpace(item.Content)
		if !stableIDPattern.MatchString(item.ID) {
			return fmt.Errorf("item %d has invalid id %q", index+1, item.ID)
		}
		if _, exists := seen[item.ID]; exists {
			return fmt.Errorf("item id %q is duplicated", item.ID)
		}
		seen[item.ID] = struct{}{}
		if item.Content == "" {
			return fmt.Errorf("item %q content is required", item.ID)
		}
		if utf8.RuneCountInString(item.Content) > maxTodoContentRunes {
			return fmt.Errorf("item %q content exceeds %d characters", item.ID, maxTodoContentRunes)
		}
		switch item.Status {
		case protocol.TodoPending, protocol.TodoCompleted, protocol.TodoCancelled:
		case protocol.TodoInProgress:
			inProgress++
		default:
			return fmt.Errorf("item %q has invalid status %q", item.ID, item.Status)
		}
	}
	if inProgress > 1 {
		return fmt.Errorf("at most one item can be in_progress")
	}
	return nil
}

func cloneTodoItems(items []protocol.TodoItem) []protocol.TodoItem {
	return append([]protocol.TodoItem(nil), items...)
}

func todoProgress(items []protocol.TodoItem) (completed, remaining int) {
	for _, item := range items {
		switch item.Status {
		case protocol.TodoCompleted:
			completed++
		case protocol.TodoPending, protocol.TodoInProgress:
			remaining++
		}
	}
	return completed, remaining
}
