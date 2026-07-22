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
	PartText        PartKind = "text"
	PartReasoning   PartKind = "reasoning"
	PartToolCall    PartKind = "tool_call"
	PartToolResult  PartKind = "tool_result"
	PartWebActivity PartKind = "web_activity"
	PartCitation    PartKind = "citation"
)

type Part struct {
	Kind        PartKind     `json:"kind"`
	Text        string       `json:"text,omitempty"`
	ToolCall    *ToolCall    `json:"tool_call,omitempty"`
	ToolResult  *ToolResult  `json:"tool_result,omitempty"`
	WebActivity *WebActivity `json:"web_activity,omitempty"`
	Citation    *URLCitation `json:"citation,omitempty"`
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
	Name            string                     `json:"name"`
	Description     string                     `json:"description"`
	InputSchema     json.RawMessage            `json:"input_schema,omitempty"`
	OutputSchema    json.RawMessage            `json:"output_schema,omitempty"`
	Annotations     *ToolAnnotations           `json:"annotations,omitempty"`
	Kind            ToolKind                   `json:"kind,omitempty"`
	Execution       ToolExecution              `json:"execution,omitempty"`
	ToolChoice      ToolChoice                 `json:"tool_choice,omitempty"`
	Fallback        ToolExecution              `json:"fallback,omitempty"`
	AllowedDomains  []string                   `json:"allowed_domains,omitempty"`
	BlockedDomains  []string                   `json:"blocked_domains,omitempty"`
	MaxUses         int                        `json:"max_uses,omitempty"`
	ContextSize     WebContextSize             `json:"context_size,omitempty"`
	UserLocation    *UserLocation              `json:"user_location,omitempty"`
	ProviderOptions map[string]json.RawMessage `json:"provider_options,omitempty"`
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
	EventResponseStart      EventKind = "response_start"
	EventReasoningDelta     EventKind = "reasoning_delta"
	EventTextDelta          EventKind = "text_delta"
	EventToolCallDelta      EventKind = "tool_call_delta"
	EventToolStart          EventKind = "tool_start"
	EventToolResult         EventKind = "tool_result"
	EventUsage              EventKind = "usage"
	EventResponseDone       EventKind = "response_done"
	EventError              EventKind = "error"
	EventWebSearchStarted   EventKind = "web_search.started"
	EventWebSearchCompleted EventKind = "web_search.completed"
	EventWebFetchStarted    EventKind = "web_fetch.started"
	EventWebFetchCompleted  EventKind = "web_fetch.completed"
	EventCitation           EventKind = "citation"
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
	WebActivity   *WebActivity   `json:"web_activity,omitempty"`
	Citation      *URLCitation   `json:"citation,omitempty"`
}

type ToolKind string

const (
	ToolFunction  ToolKind = "function"
	ToolWebSearch ToolKind = "web_search"
	ToolWebFetch  ToolKind = "web_fetch"
)

func (kind ToolKind) Effective() ToolKind {
	if kind == "" {
		return ToolFunction
	}
	return kind
}

func (kind ToolKind) IsWeb() bool { return kind == ToolWebSearch || kind == ToolWebFetch }

type ToolExecution string

const (
	ExecutionAuto      ToolExecution = "auto"
	ExecutionHosted    ToolExecution = "hosted"
	ExecutionDelegated ToolExecution = "delegated"
	ExecutionClient    ToolExecution = "client"
)

func (execution ToolExecution) Effective() ToolExecution {
	if execution == "" {
		return ExecutionAuto
	}
	return execution
}

type ToolChoice string

const (
	ToolChoiceAuto     ToolChoice = "auto"
	ToolChoiceRequired ToolChoice = "required"
	ToolChoiceNone     ToolChoice = "none"
)

func (choice ToolChoice) Effective() ToolChoice {
	if choice == "" {
		return ToolChoiceAuto
	}
	return choice
}

type WebContextSize string

const (
	WebContextLow    WebContextSize = "low"
	WebContextMedium WebContextSize = "medium"
	WebContextHigh   WebContextSize = "high"
)

func (size WebContextSize) Effective() WebContextSize {
	if size == "" {
		return WebContextMedium
	}
	return size
}

type UserLocation struct {
	Country  string `json:"country,omitempty"`
	Region   string `json:"region,omitempty"`
	City     string `json:"city,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

type WebStatus string

const (
	WebStatusPending   WebStatus = "pending"
	WebStatusRunning   WebStatus = "running"
	WebStatusCompleted WebStatus = "completed"
	WebStatusError     WebStatus = "error"
)

type WebSource struct {
	URL     string `json:"url"`
	Title   string `json:"title,omitempty"`
	Snippet string `json:"snippet,omitempty"`
}

type WebUsage struct {
	Searches     int     `json:"searches,omitempty"`
	Fetches      int     `json:"fetches,omitempty"`
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

type WebActivity struct {
	CallID              string                     `json:"call_id"`
	Kind                ToolKind                   `json:"kind"`
	Query               string                     `json:"query,omitempty"`
	URL                 string                     `json:"url,omitempty"`
	Action              string                     `json:"action,omitempty"`
	Status              WebStatus                  `json:"status"`
	Sources             []WebSource                `json:"sources,omitempty"`
	DurationMS          int64                      `json:"duration_ms,omitempty"`
	Usage               WebUsage                   `json:"usage,omitzero"`
	ProviderMetadata    map[string]json.RawMessage `json:"provider_metadata,omitempty"`
	RawProviderResponse json.RawMessage            `json:"raw_provider_response,omitempty"`
	RawTruncated        bool                       `json:"raw_truncated,omitempty"`
	Error               string                     `json:"error,omitempty"`
}

type URLCitation struct {
	CallID           string                     `json:"call_id,omitempty"`
	URL              string                     `json:"url"`
	Title            string                     `json:"title,omitempty"`
	StartIndex       int                        `json:"start_index,omitempty"`
	EndIndex         int                        `json:"end_index,omitempty"`
	Summary          string                     `json:"summary,omitempty"`
	ProviderMetadata map[string]json.RawMessage `json:"provider_metadata,omitempty"`
}
