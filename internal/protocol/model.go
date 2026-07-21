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

type ContentType string

const (
	ContentText             ContentType = "text"
	ContentImage            ContentType = "image"
	ContentAudio            ContentType = "audio"
	ContentEmbeddedResource ContentType = "resource"
	ContentResourceLink     ContentType = "resource_link"
)

type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  bool   `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

type ContentAnnotations struct {
	Audience     []string `json:"audience,omitempty"`
	Priority     float64  `json:"priority,omitempty"`
	LastModified string   `json:"lastModified,omitempty"`
}

type Icon struct {
	Source   string   `json:"src"`
	MIMEType string   `json:"mimeType,omitempty"`
	Sizes    []string `json:"sizes,omitempty"`
	Theme    string   `json:"theme,omitempty"`
}

type ResourceContents struct {
	URI      string         `json:"uri"`
	MIMEType string         `json:"mimeType,omitempty"`
	Text     string         `json:"text,omitempty"`
	Blob     []byte         `json:"blob,omitempty"`
	Meta     map[string]any `json:"_meta,omitempty"`
}

// ContentBlock mirrors the MCP content variants while ToolResult.Content keeps
// the legacy text rendering consumed by existing drivers and sessions.
type ContentBlock struct {
	Type        ContentType         `json:"type"`
	Text        string              `json:"text,omitempty"`
	Data        []byte              `json:"data,omitempty"`
	MIMEType    string              `json:"mimeType,omitempty"`
	Resource    *ResourceContents   `json:"resource,omitempty"`
	URI         string              `json:"uri,omitempty"`
	Name        string              `json:"name,omitempty"`
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	Size        *int64              `json:"size,omitempty"`
	Icons       []Icon              `json:"icons,omitempty"`
	Meta        map[string]any      `json:"_meta,omitempty"`
	Annotations *ContentAnnotations `json:"annotations,omitempty"`
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
	CallID            string          `json:"call_id"`
	Content           string          `json:"content"`
	ContentBlocks     []ContentBlock  `json:"content_blocks,omitempty"`
	StructuredContent json.RawMessage `json:"structured_content,omitempty"`
	IsError           bool            `json:"is_error,omitempty"`
	Truncated         bool            `json:"truncated,omitempty"`
	Metadata          map[string]any  `json:"metadata,omitempty"`
	TodoList          *TodoList       `json:"todo_list,omitempty"`
}

type ToolDefinition struct {
	Name         string           `json:"name"`
	Description  string           `json:"description"`
	InputSchema  json.RawMessage  `json:"input_schema"`
	OutputSchema json.RawMessage  `json:"output_schema,omitempty"`
	Annotations  *ToolAnnotations `json:"annotations,omitempty"`
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
