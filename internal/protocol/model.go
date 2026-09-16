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
	// ParentCallID records the model call that produced this call when the host
	// expanded one model call into several executions. It is host-owned and never
	// taken from the model, so a provider never sees it and no caller has to parse
	// a string to recover the parent relation.
	ParentCallID string `json:"parent_call_id,omitempty"`
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

// CallState is the terminal execution state of one tool call.
//
// The values are assigned by the host executor so that "not executed", "failed",
// "rejected", "cancelled" and "result unknown" stay distinguishable. Tool
// content, MCP annotations and other untrusted metadata never decide them.
type CallState string

const (
	// CallSucceeded means the call ran and reported success.
	CallSucceeded CallState = "succeeded"
	// CallFailed means the call ran, or was prepared, and reported a failure the
	// model is expected to adjust to.
	CallFailed CallState = "failed"
	// CallRejected means policy or the user refused the call before execution.
	CallRejected CallState = "rejected"
	// CallCancelled means the call was cancelled around its execution.
	CallCancelled CallState = "cancelled"
	// CallNotExecuted means the call was never started.
	CallNotExecuted CallState = "not_executed"
	// CallOutcomeUnknown means the call may have produced a side effect but its
	// result could not be confirmed. It is never retried automatically.
	CallOutcomeUnknown CallState = "outcome_unknown"
)

// BatchControl is the request-level control state of one tool batch. It is
// derived by the executor and the approval layer only, never from tool content
// or external metadata.
type BatchControl string

const (
	// ControlContinue lets the model adjust to ordinary tool failures.
	ControlContinue BatchControl = "continue"
	// ControlInterruptRequest records a user refusal without a reason.
	ControlInterruptRequest BatchControl = "interrupt_request"
	// ControlCancelRequest records that the request context was cancelled.
	ControlCancelRequest BatchControl = "cancel_request"
	// ControlAbortRequest records an approval-channel or execution
	// infrastructure failure.
	ControlAbortRequest BatchControl = "abort_request"
)

// BatchOutcome is the request-level control state of one tool batch.
type BatchOutcome struct {
	// Control is the highest-precedence control state observed in the batch.
	// Infrastructure failures outrank a request cancellation, which outranks a
	// user interruption request, which outranks a normal continuation.
	Control BatchControl
	// Cause is the primary failure with any additional causes joined, so that
	// errors.Is holds for each of them. It is nil for ControlContinue and
	// ControlInterruptRequest, which are not failures.
	Cause error
	// States lists the terminal state of each call in request order.
	States []CallState
}

// Err returns the failure that ends the request, if any. A user interruption is
// not an error: the caller decides how to surface it.
func (o BatchOutcome) Err() error {
	switch o.Control {
	case ControlCancelRequest, ControlAbortRequest:
		return o.Cause
	default:
		return nil
	}
}

// LineRange is one retained 1-based inclusive line range of a code slice. A
// trimmed fragment reports exactly which file lines survived, so a reference is
// only used when the retained body really covers the lines it points at.
type LineRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type ToolResult struct {
	CallID            string          `json:"call_id"`
	Content           string          `json:"content"`
	ContentBlocks     []ContentBlock  `json:"content_blocks,omitempty"`
	StructuredContent json.RawMessage `json:"structured_content,omitempty"`
	IsError           bool            `json:"is_error,omitempty"`
	Truncated         bool            `json:"truncated,omitempty"`
	// State is the terminal execution state assigned by the host executor.
	State    CallState      `json:"call_state,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	TodoList *TodoList      `json:"todo_list,omitempty"`
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
	// CachedInputTokens is the part of InputTokens the provider served from its
	// own cache. It is a subset, never an addition: every supported provider
	// counts a cached prompt token in its input tokens as well, so adding this to
	// the totals would count it twice. It is reported so cost accounting can tell
	// a cache hit from a miss, and it stays 0 when the provider does not report
	// it, which leaves Exact unaffected.
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`
}

type ModelResponse struct {
	Turn        Turn            `json:"turn"`
	Stop        StopKind        `json:"stop"`
	Usage       Usage           `json:"usage"`
	DriverState json.RawMessage `json:"driver_state,omitempty"`
	// Interop names every deliberate relaxation of the interoperability policy
	// this response needed. An empty list means the provider behaved as the
	// client expects; a named entry means the run report and the audit trail
	// must say so.
	Interop []string `json:"interop,omitempty"`
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
	EventWebSearchUpdated   EventKind = "web_search.updated"
	EventWebSearchCompleted EventKind = "web_search.completed"
	EventWebFetchStarted    EventKind = "web_fetch.started"
	EventWebFetchUpdated    EventKind = "web_fetch.updated"
	EventWebFetchCompleted  EventKind = "web_fetch.completed"
	EventCitation           EventKind = "citation"
	EventAgentTaskUpdated   EventKind = "agent_task.updated"
)

type AgentTaskActivity struct {
	TaskID       string          `json:"task_id"`
	SubagentType string          `json:"subagent_type"`
	Status       string          `json:"status"`
	Background   bool            `json:"background"`
	Error        string          `json:"error,omitempty"`
	Report       json.RawMessage `json:"report,omitempty"`
}

type ModelEvent struct {
	Kind          EventKind          `json:"kind"`
	Delta         string             `json:"delta,omitempty"`
	ToolCallDelta *ToolCallDelta     `json:"tool_call_delta,omitempty"`
	ToolCall      *ToolCall          `json:"tool_call,omitempty"`
	ToolResult    *ToolResult        `json:"tool_result,omitempty"`
	Usage         *Usage             `json:"usage,omitempty"`
	Response      *ModelResponse     `json:"response,omitempty"`
	Error         *Error             `json:"error,omitempty"`
	WebActivity   *WebActivity       `json:"web_activity,omitempty"`
	Citation      *URLCitation       `json:"citation,omitempty"`
	AgentTask     *AgentTaskActivity `json:"agent_task,omitempty"`
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
	Queries             []string                   `json:"queries,omitempty"`
	URL                 string                     `json:"url,omitempty"`
	Pattern             string                     `json:"pattern,omitempty"`
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
