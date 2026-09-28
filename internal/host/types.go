// Package host implements the versioned Bastion stdio boundary. None of its
// exported wire types contain provider credentials or local workspace settings.
package host

import "encoding/json"

const ProtocolVersion = "bastion-host/1.0"
const StateSchema = "eylu-host-state/1"
const MaxMessageBytes = 16 << 20

type Initialize struct {
	ProtocolVersion      string   `json:"protocol_version"`
	HostName             string   `json:"host_name"`
	HostVersion          string   `json:"host_version"`
	MaxMessageBytes      int      `json:"max_message_bytes"`
	RequiredCapabilities []string `json:"required_capabilities"`
	OptionalCapabilities []string `json:"optional_capabilities"`
}
type Initialized struct {
	ProtocolVersion string   `json:"protocol_version"`
	EngineName      string   `json:"engine_name"`
	EngineVersion   string   `json:"engine_version"`
	MaxMessageBytes int      `json:"max_message_bytes"`
	Capabilities    []string `json:"capabilities"`
	StateSchema     string   `json:"state_schema"`
}
type ContextItem struct {
	Source string `json:"source"`
	Text   string `json:"text"`
}
type Create struct {
	ConversationID string        `json:"conversation_id"`
	Instructions   string        `json:"instructions"`
	Context        []ContextItem `json:"context"`
}
type Binding struct {
	BindingID       string `json:"binding_id"`
	BindingRevision int64  `json:"binding_revision"`
}
type ModelBinding struct {
	Binding
	ContextWindowTokens int      `json:"context_window_tokens"`
	MaxOutputTokens     int      `json:"max_output_tokens"`
	Capabilities        []string `json:"capabilities"`
}
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	ReadOnly    *bool           `json:"read_only,omitempty"`
}
type ToolCatalog struct {
	Binding
	Tools []ToolDefinition `json:"tools"`
}
type Limits struct {
	MaxModelCalls        int   `json:"max_model_calls"`
	MaxOutputTokensTotal int   `json:"max_output_tokens_total"`
	MaxElapsedMS         int64 `json:"max_elapsed_ms"`
	CallbackTimeoutMS    int64 `json:"callback_timeout_ms"`
	StopGraceMS          int64 `json:"stop_grace_ms"`
}
type TextInput struct {
	Text string `json:"text"`
}
type Start struct {
	ConversationID   string       `json:"conversation_id"`
	ExpectedRevision int64        `json:"expected_revision"`
	RunID            string       `json:"run_id"`
	Input            TextInput    `json:"input"`
	ModelBinding     ModelBinding `json:"model_binding"`
	ToolCatalog      ToolCatalog  `json:"tool_catalog"`
	TargetBinding    Binding      `json:"target_binding"`
	Limits           Limits       `json:"limits"`
}
type Get struct {
	ConversationID string `json:"conversation_id"`
}
type Interrupt struct {
	ConversationID string `json:"conversation_id"`
	RunID          string `json:"run_id"`
	Reason         string `json:"reason"`
}
type Shutdown struct {
	Reason string `json:"reason"`
	WaitMS int64  `json:"wait_ms"`
}
type Accepted struct {
	ConversationID string `json:"conversation_id"`
	RunID          string `json:"run_id,omitempty"`
	Revision       int64  `json:"revision"`
}
type Part struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	Result     *ToolResult     `json:"result,omitempty"`
}
type Message struct {
	Role  string `json:"role"`
	Parts []Part `json:"parts"`
}
type StoredMessage struct {
	MessageID string  `json:"message_id"`
	Message   Message `json:"message"`
}
type Usage struct {
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	Source       string `json:"source"`
}
type RemainingLimits struct {
	ModelCalls      int   `json:"model_calls"`
	OutputTokens    int   `json:"output_tokens"`
	ElapsedMS       int64 `json:"elapsed_ms"`
	MaxOutputTokens int   `json:"max_output_tokens"`
}
type ModelRequest struct {
	ConversationID  string           `json:"conversation_id"`
	RunID           string           `json:"run_id"`
	RequestID       string           `json:"request_id"`
	ModelBinding    ModelBinding     `json:"model_binding"`
	Purpose         string           `json:"purpose"`
	Messages        []Message        `json:"messages"`
	Tools           []ToolDefinition `json:"tools"`
	RemainingLimits RemainingLimits  `json:"remaining_limits"`
}
type ModelResult struct {
	RequestID    string  `json:"request_id"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
	Usage        Usage   `json:"usage"`
}
type ToolRequest struct {
	ConversationID string          `json:"conversation_id"`
	RunID          string          `json:"run_id"`
	RequestID      string          `json:"request_id"`
	ToolCallID     string          `json:"tool_call_id"`
	ToolCatalog    Binding         `json:"tool_catalog"`
	TargetBinding  Binding         `json:"target_binding"`
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments"`
}
type SafeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type ToolResult struct {
	RequestID        string     `json:"request_id"`
	OperationID      *string    `json:"operation_id"`
	Status           string     `json:"status"`
	Effect           string     `json:"effect"`
	Content          []Part     `json:"content"`
	Error            *SafeError `json:"error"`
	Truncated        bool       `json:"truncated"`
	ResourceRef      string     `json:"resource_ref,omitempty"`
	TruncationReason string     `json:"truncation_reason,omitempty"`
}
type CancelCalls struct {
	ConversationID string   `json:"conversation_id"`
	RunID          string   `json:"run_id"`
	RequestIDs     []string `json:"request_ids"`
	Reason         string   `json:"reason"`
}
type Delta struct {
	ConversationID string `json:"conversation_id"`
	RunID          string `json:"run_id"`
	RequestID      string `json:"request_id"`
	DeltaSeq       int64  `json:"delta_seq"`
	Text           string `json:"text"`
}
type CallProgress struct {
	ConversationID string  `json:"conversation_id"`
	RunID          string  `json:"run_id"`
	RequestID      string  `json:"request_id"`
	Phase          string  `json:"phase"`
	OperationID    *string `json:"operation_id,omitempty"`
}
type Event struct {
	EventID string          `json:"event_id"`
	Type    string          `json:"type"`
	RunID   string          `json:"run_id,omitempty"`
	Data    json.RawMessage `json:"data"`
}
type SeqRange struct {
	First int64 `json:"first"`
	Last  int64 `json:"last"`
}
type Commit struct {
	ConversationID   string  `json:"conversation_id"`
	CheckpointID     string  `json:"checkpoint_id"`
	ExpectedRevision int64   `json:"expected_revision"`
	BaseEventSeq     int64   `json:"base_event_seq"`
	StateSchema      string  `json:"state_schema"`
	State            State   `json:"state"`
	Events           []Event `json:"events"`
	Reason           string  `json:"reason"`
}
type Committed struct {
	Revision      int64     `json:"revision"`
	EventSeq      int64     `json:"event_seq"`
	EventSeqRange *SeqRange `json:"event_seq_range"`
}
type CommittedNotification struct {
	ConversationID string `json:"conversation_id"`
	CheckpointID   string `json:"checkpoint_id"`
	Committed
}
type Run struct {
	RunID                string   `json:"run_id"`
	Status               string   `json:"status"`
	Phase                string   `json:"phase,omitempty"`
	Reason               string   `json:"reason,omitempty"`
	BlockingReason       string   `json:"blocking_reason,omitempty"`
	Usage                Usage    `json:"usage"`
	ModelCalls           int      `json:"model_calls"`
	KnownEffects         []string `json:"known_effects"`
	UnresolvedRequestIDs []string `json:"unresolved_request_ids"`
}
type RunRecord struct {
	Digest   string   `json:"digest"`
	Accepted Accepted `json:"accepted"`
	Outcome  *Run     `json:"outcome"`
}
type PendingCall struct {
	RequestID   string          `json:"request_id"`
	RunID       string          `json:"run_id"`
	Kind        string          `json:"kind"`
	Digest      string          `json:"digest"`
	Request     json.RawMessage `json:"request"`
	ModelResult *ModelResult    `json:"model_result,omitempty"`
	ToolResult  *ToolResult     `json:"tool_result,omitempty"`
}

// State is a separate storage DTO; internal agent state is explicitly projected
// into messages and summary and is never serialized as a compatibility promise.
type State struct {
	ConversationID  string               `json:"conversation_id"`
	Revision        int64                `json:"revision"`
	EventSeq        int64                `json:"event_seq"`
	CreateDigest    string               `json:"create_digest"`
	Instructions    string               `json:"instructions"`
	Context         []ContextItem        `json:"context"`
	Messages        []StoredMessage      `json:"messages"`
	Summary         string               `json:"summary"`
	ActiveRun       *Run                 `json:"active_run"`
	LastRun         *Run                 `json:"last_run"`
	RunInput        *Start               `json:"run_input"`
	Runs            map[string]RunRecord `json:"runs"`
	PendingCalls    []PendingCall        `json:"pending_calls"`
	UsedToolCallIDs []string             `json:"used_tool_call_ids"`
}
type View struct {
	ConversationID         string   `json:"conversation_id"`
	Revision               int64    `json:"revision"`
	EventSeq               int64    `json:"event_seq"`
	ActiveRun              *Run     `json:"active_run"`
	LastRun                *Run     `json:"last_run"`
	HasUncommittedProgress bool     `json:"has_uncommitted_progress"`
	BlockingRequestIDs     []string `json:"blocking_request_ids"`
}
type Reconciliation struct {
	RequestID   string       `json:"request_id"`
	Status      string       `json:"status"`
	ModelResult *ModelResult `json:"model_result,omitempty"`
	ToolResult  *ToolResult  `json:"tool_result,omitempty"`
}
type Restore struct {
	ConversationID  string           `json:"conversation_id"`
	Revision        int64            `json:"revision"`
	EventSeq        int64            `json:"event_seq"`
	StateSchema     string           `json:"state_schema"`
	State           State            `json:"state"`
	Reconciliations []Reconciliation `json:"reconciliations"`
}
