package ui

import (
	"context"
	"errors"
	"io"
	"time"

	tea "charm.land/bubbletea/v2"

	contextledger "Eylu/internal/context"
	"Eylu/internal/protocol"
)

type OperationState string

const (
	StateIdle              OperationState = "idle"
	StateConnecting        OperationState = "connecting"
	StateFetchingModels    OperationState = "fetching_models"
	StateWaitingFirstToken OperationState = "waiting_first_token"
	StateStreaming         OperationState = "streaming"
	StatePreparingTool     OperationState = "preparing_tool"
	StateExecutingTool     OperationState = "executing_tool"
	StateAwaitingApproval  OperationState = "awaiting_approval"
	StateAwaitingInput     OperationState = "awaiting_input"
	StateRetryBackoff      OperationState = "retry_backoff"
	StateCancelling        OperationState = "cancelling"
	StateCancelled         OperationState = "cancelled"
	StateInterrupted       OperationState = "interrupted"
	StateCompleted         OperationState = "completed"
	StateFailed            OperationState = "failed"
)

var ErrRequestInterrupted = errors.New("request interrupted by user")

type EventKind string

const (
	EventState          EventKind = "state"
	EventActivity       EventKind = "activity"
	EventReasoningDelta EventKind = "reasoning_delta"
	EventTextDelta      EventKind = "text_delta"
	EventToolCallDelta  EventKind = "tool_call_delta"
	EventToolStart      EventKind = "tool_start"
	EventToolResult     EventKind = "tool_result"
	EventToolAudit      EventKind = "tool_audit"
	EventApproval       EventKind = "approval"
	EventAsk            EventKind = "ask"
	EventContext        EventKind = "context"
	EventUsage          EventKind = "usage"
	EventNotice         EventKind = "notice"
)

type Activity struct {
	Reasoning          bool `json:"reasoning"`
	ReasoningKnown     bool `json:"reasoning_known,omitempty"`
	TokenBytesPerToken int  `json:"token_bytes_per_token"`
	InputTokens        int  `json:"input_tokens,omitempty"`
	InputExact         bool `json:"input_exact,omitempty"`
}

type Event struct {
	OperationID   string                  `json:"operation_id"`
	Kind          EventKind               `json:"kind"`
	State         OperationState          `json:"state,omitempty"`
	Activity      *Activity               `json:"activity,omitempty"`
	Delta         string                  `json:"delta,omitempty"`
	ToolCallDelta *protocol.ToolCallDelta `json:"tool_call_delta,omitempty"`
	ToolCall      *protocol.ToolCall      `json:"tool_call,omitempty"`
	ToolResult    *protocol.ToolResult    `json:"tool_result,omitempty"`
	ToolAudit     *ToolAudit              `json:"tool_audit,omitempty"`
	Approval      *ApprovalRequest        `json:"-"`
	Ask           *AskRequest             `json:"-"`
	Context       *contextledger.Report   `json:"context,omitempty"`
	Usage         *protocol.Usage         `json:"usage,omitempty"`
	Notice        string                  `json:"notice,omitempty"`
	Error         bool                    `json:"error,omitempty"`
	RetryAfter    time.Duration           `json:"retry_after,omitempty"`
}

type ToolAudit struct {
	CallID     string `json:"call_id"`
	DurationMS int64  `json:"duration_ms"`
	Decision   string `json:"decision"`
	Risk       string `json:"risk"`
	ExitCode   int    `json:"exit_code,omitempty"`
}

type ApprovalRequest struct {
	Tool         string
	Risk         string
	Summary      string
	Reason       string
	PolicyReason string
	Warning      bool
	Step         int
	Total        int
	Response     chan ApprovalDecision
}

type ApprovalDecision struct {
	Approved bool
	Reason   string
}

type AskRequest struct {
	Questions []protocol.AskQuestion
	Response  chan AskDecision
}

type AskDecision struct {
	Answers   map[string][]string
	Cancelled bool
}

type ProviderItem struct {
	Name          string `json:"name"`
	Adapter       string `json:"adapter"`
	BaseURL       string `json:"base_url"`
	Model         string `json:"model"`
	ContextWindow int    `json:"context_window,omitempty"`
	Active        bool   `json:"active"`
}

type SkillItem struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Status      string `json:"status"`
	ShadowedBy  string `json:"shadowed_by,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Activated   bool   `json:"activated"`
}

type ReferenceKind string

const (
	ReferenceSkill ReferenceKind = "skill"
	ReferenceFile  ReferenceKind = "file"
)

type Reference struct {
	Kind  ReferenceKind `json:"kind"`
	Value string        `json:"value"`
}

type Submission struct {
	Text       string      `json:"text"`
	References []Reference `json:"references,omitempty"`
}

type FileItem struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type Snapshot struct {
	SessionID string               `json:"session_id"`
	Workspace string               `json:"workspace"`
	Mode      string               `json:"mode"`
	Provider  string               `json:"provider"`
	Model     string               `json:"model"`
	Context   contextledger.Report `json:"context"`
	Providers []ProviderItem       `json:"providers"`
	Skills    []SkillItem          `json:"skills"`
	TodoList  protocol.TodoList    `json:"todo_list,omitzero"`
}

type ProviderForm struct {
	OriginalName  string `json:"original_name,omitempty"`
	Name          string `json:"name"`
	BaseURL       string `json:"base_url"`
	Model         string `json:"model"`
	Adapter       string `json:"adapter"`
	APIKey        string `json:"-"`
	ContextWindow int    `json:"context_window,omitempty"`
}

type Backend interface {
	Snapshot(context.Context) (Snapshot, error)
	Submit(context.Context, string, Submission, func(Event)) error
	Command(context.Context, string) (string, error)
	ListFiles(context.Context) ([]FileItem, error)
	SetMode(context.Context, string) error
	UpsertProvider(context.Context, ProviderForm) error
	DeleteProvider(context.Context, string) error
	UseProvider(context.Context, string) error
	SetModel(context.Context, string, string) error
	FetchModels(context.Context, string) ([]string, error)
}

type Clock interface {
	Now() time.Time
	Tick(time.Duration, func(time.Time) tea.Msg) tea.Cmd
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) Tick(duration time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
	return tea.Tick(duration, fn)
}

type Options struct {
	Context        context.Context
	Input          io.Reader
	Output         io.Writer
	NoAnimation    bool
	NoColor        bool
	Clock          Clock
	Width          int
	Height         int
	ClipboardWrite func(string) error
}
