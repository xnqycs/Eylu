package protocol

import (
	"encoding/json"
	"time"
)

const Version = 1

type Role string

const (
	RoleSystem Role = "system"
	RoleUser   Role = "user"
	RoleAgent  Role = "agent"
	RoleTool   Role = "tool"
)

type PartKind string

const (
	PartText       PartKind = "text"
	PartReasoning  PartKind = "reasoning"
	PartToolCall   PartKind = "tool_call"
	PartToolResult PartKind = "tool_result"
)

type Part struct {
	Kind       PartKind    `json:"kind"`
	Text       string      `json:"text,omitempty"`
	ToolCall   *ToolCall   `json:"tool_call,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
}

type Turn struct {
	ID        string    `json:"id"`
	Role      Role      `json:"role"`
	Parts     []Part    `json:"parts"`
	CreatedAt time.Time `json:"created_at"`
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ToolCallDelta struct {
	OutputIndex int    `json:"output_index"`
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	Delta       string `json:"delta,omitempty"`
	Arguments   string `json:"arguments,omitempty"`
	Done        bool   `json:"done,omitempty"`
}

type TodoStatus string

const (
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoCompleted  TodoStatus = "completed"
	TodoCancelled  TodoStatus = "cancelled"
)

type TodoItem struct {
	ID      string     `json:"id"`
	Content string     `json:"content"`
	Status  TodoStatus `json:"status"`
}

type TodoList struct {
	Explanation string     `json:"explanation,omitempty"`
	Items       []TodoItem `json:"items"`
}

func (list TodoList) IsZero() bool {
	return list.Explanation == "" && len(list.Items) == 0
}

type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type AskQuestion struct {
	ID       string      `json:"id"`
	Header   string      `json:"header"`
	Question string      `json:"question"`
	Multiple bool        `json:"multiple,omitempty"`
	Options  []AskOption `json:"options"`
}

type AskRequest struct {
	Questions []AskQuestion `json:"questions"`
}

type AskResponse struct {
	Answers map[string][]string `json:"answers"`
}

type ToolResult struct {
	CallID    string         `json:"call_id"`
	Content   string         `json:"content"`
	IsError   bool           `json:"is_error,omitempty"`
	Truncated bool           `json:"truncated,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	TodoList  *TodoList      `json:"todo_list,omitempty"`
}

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type ModelRequest struct {
	ProtocolVersion int              `json:"protocol_version"`
	Model           string           `json:"model"`
	Turns           []Turn           `json:"turns"`
	Tools           []ToolDefinition `json:"tools,omitempty"`
	DriverState     json.RawMessage  `json:"driver_state,omitempty"`
}

type StopKind string

const (
	StopCompleted StopKind = "completed"
	StopToolUse   StopKind = "tool_use"
	StopLength    StopKind = "length"
	StopCancelled StopKind = "cancelled"
	StopError     StopKind = "error"
)

type Usage struct {
	InputTokens     int  `json:"input_tokens"`
	OutputTokens    int  `json:"output_tokens"`
	ReasoningTokens int  `json:"reasoning_tokens,omitempty"`
	Exact           bool `json:"exact"`
}

type ModelResponse struct {
	Turn        Turn            `json:"turn"`
	Stop        StopKind        `json:"stop"`
	Usage       Usage           `json:"usage"`
	DriverState json.RawMessage `json:"driver_state,omitempty"`
}

type EventKind string

const (
	EventResponseStart  EventKind = "response_start"
	EventReasoningDelta EventKind = "reasoning_delta"
	EventTextDelta      EventKind = "text_delta"
	EventToolCallDelta  EventKind = "tool_call_delta"
	EventToolStart      EventKind = "tool_start"
	EventToolResult     EventKind = "tool_result"
	EventUsage          EventKind = "usage"
	EventResponseDone   EventKind = "response_done"
	EventError          EventKind = "error"
)

type ModelEvent struct {
	Kind          EventKind      `json:"kind"`
	Delta         string         `json:"delta,omitempty"`
	ToolCallDelta *ToolCallDelta `json:"tool_call_delta,omitempty"`
	ToolCall      *ToolCall      `json:"tool_call,omitempty"`
	ToolResult    *ToolResult    `json:"tool_result,omitempty"`
	Usage         *Usage         `json:"usage,omitempty"`
	Response      *ModelResponse `json:"response,omitempty"`
	Error         *Error         `json:"error,omitempty"`
}
