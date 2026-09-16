package session

import (
	"encoding/json"
	"time"

	contextledger "Eylu/internal/context"
	"Eylu/internal/environment"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

// SchemaVersion 3 adds the stable event ID and the tool lifecycle events. A
// version 2 document is still readable because the new fields are additive, while
// anything outside the supported range is refused explicitly.
const SchemaVersion = 3

// MinReadableSchemaVersion is the oldest document this build reads. An older
// document is refused rather than guessed at.
const MinReadableSchemaVersion = 2

type EventType string

const (
	EventSessionCreated    EventType = "session_created"
	EventTurnAppended      EventType = "turn_appended"
	EventPromptRecorded    EventType = "prompt_recorded"
	EventRuntimeUpdated    EventType = "runtime_updated"
	EventDriverState       EventType = "driver_state_updated"
	EventSkillActivated    EventType = "skill_activated"
	EventContextUpdated    EventType = "context_updated"
	EventAgentTasksUpdated EventType = "agent_tasks_updated"
	EventErrorRecorded     EventType = "error_recorded"
	EventSessionClosed     EventType = "session_closed"
	EventSessionReopened   EventType = "session_reopened"
	// EventToolExecutionIntent records that a side-effecting execution is about
	// to start. It is written before the tool runs, so its presence is evidence
	// that the operation may have happened.
	EventToolExecutionIntent EventType = "tool_execution_intent"
	// EventToolCompleted records the terminal outcome of one execution.
	EventToolCompleted EventType = "tool_completed"
)

// ToolIntent is the durable record of one execution that is about to start.
//
// TargetPath and PreviousHash are verifiable recovery hints for file tools: they
// help decide whether the operation happened. They are never used to replay it.
type ToolIntent struct {
	RequestID    string    `json:"request_id,omitempty"`
	CallID       string    `json:"call_id"`
	ParentCallID string    `json:"parent_call_id,omitempty"`
	Tool         string    `json:"tool"`
	Risk         string    `json:"risk,omitempty"`
	TargetPath   string    `json:"target_path,omitempty"`
	PreviousHash string    `json:"previous_hash,omitempty"`
	StartedAt    time.Time `json:"started_at,omitzero"`
}

// ToolCompletion is the durable record of one finished execution.
type ToolCompletion struct {
	CallID      string    `json:"call_id"`
	Tool        string    `json:"tool"`
	State       string    `json:"state,omitempty"`
	IsError     bool      `json:"is_error,omitempty"`
	TargetPath  string    `json:"target_path,omitempty"`
	ResultHash  string    `json:"result_hash,omitempty"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
}

type ProviderState struct {
	Name                   string    `json:"name"`
	Generation             uint64    `json:"generation"`
	Adapter                string    `json:"adapter"`
	BaseURL                string    `json:"base_url"`
	Model                  string    `json:"model"`
	ReasoningEffort        string    `json:"reasoning_effort,omitempty"`
	CatalogProvider        string    `json:"catalog_provider,omitempty"`
	ContextWindow          int       `json:"context_window,omitempty"`
	DetectedContextWindow  int       `json:"detected_context_window,omitempty"`
	EffectiveContextWindow int       `json:"effective_context_window,omitempty"`
	LimitSource            string    `json:"limit_source,omitempty"`
	LimitObservedAt        time.Time `json:"limit_observed_at,omitzero"`
	LimitCached            bool      `json:"limit_cached,omitempty"`
	LimitAssumed           bool      `json:"limit_assumed,omitempty"`
	LimitDegradations      int       `json:"limit_degradations,omitempty"`
}

type SkillState struct {
	Name         string    `json:"name"`
	Source       string    `json:"source"`
	Entry        string    `json:"entry"`
	Root         string    `json:"root"`
	Digest       string    `json:"digest"`
	Trigger      string    `json:"trigger"`
	ActivatedAt  time.Time `json:"activated_at"`
	AllowedTools string    `json:"allowed_tools,omitempty"`
}

type Snapshot struct {
	Version        int                       `json:"version"`
	Sequence       uint64                    `json:"sequence"`
	SessionID      string                    `json:"session_id"`
	CreatedAt      time.Time                 `json:"created_at"`
	UpdatedAt      time.Time                 `json:"updated_at"`
	ClosedAt       *time.Time                `json:"closed_at,omitempty"`
	Workspace      string                    `json:"workspace"`
	Environment    environment.Context       `json:"environment,omitzero"`
	PermissionMode string                    `json:"permission_mode"`
	Provider       ProviderState             `json:"provider"`
	Turns          []protocol.Turn           `json:"turns"`
	PromptHistory  []string                  `json:"prompt_history"`
	DriverState    json.RawMessage           `json:"driver_state,omitempty"`
	SkillCatalog   string                    `json:"skill_catalog,omitempty"`
	Skills         []SkillState              `json:"skills,omitempty"`
	Summary        string                    `json:"summary,omitempty"`
	TodoList       protocol.TodoList         `json:"todo_list,omitzero"`
	AgentTasks     []tool.AgentTask          `json:"agent_tasks,omitempty"`
	OmittedTurnIDs []string                  `json:"omitted_turn_ids,omitempty"`
	Ledger         contextledger.LedgerState `json:"ledger"`
	LastError      string                    `json:"last_error,omitempty"`
	// PendingIntents lists executions that started but never recorded a terminal
	// outcome. Recovery reports them as outcome_unknown and never replays them.
	PendingIntents []ToolIntent `json:"pending_intents,omitempty"`
}

type Event struct {
	Version int `json:"version"`
	// ID is the stable identity of one logical event. A retry after an uncertain
	// append reuses it, so the log can recognize the duplicate instead of writing
	// the same logical change twice. An event written before IDs existed simply
	// has no ID.
	ID             string                     `json:"id,omitempty"`
	Sequence       uint64                     `json:"sequence"`
	Type           EventType                  `json:"type"`
	SessionID      string                     `json:"session_id"`
	At             time.Time                  `json:"at"`
	Workspace      string                     `json:"workspace,omitempty"`
	Environment    *environment.Context       `json:"environment,omitempty"`
	PermissionMode string                     `json:"permission_mode,omitempty"`
	Provider       *ProviderState             `json:"provider,omitempty"`
	Turn           *protocol.Turn             `json:"turn,omitempty"`
	Prompt         string                     `json:"prompt,omitempty"`
	DriverState    json.RawMessage            `json:"driver_state,omitempty"`
	Skill          *SkillState                `json:"skill,omitempty"`
	SkillCatalog   string                     `json:"skill_catalog,omitempty"`
	Summary        string                     `json:"summary,omitempty"`
	TodoList       *protocol.TodoList         `json:"todo_list,omitempty"`
	AgentTasks     []tool.AgentTask           `json:"agent_tasks,omitempty"`
	OmittedTurnIDs []string                   `json:"omitted_turn_ids,omitempty"`
	Ledger         *contextledger.LedgerState `json:"ledger,omitempty"`
	Error          string                     `json:"error,omitempty"`
	Intent         *ToolIntent                `json:"intent,omitempty"`
	Completion     *ToolCompletion            `json:"completion,omitempty"`
}

type Diagnostic struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

type SessionInfo struct {
	SessionID  string     `json:"session_id"`
	Workspace  string     `json:"workspace"`
	Mode       string     `json:"mode"`
	Provider   string     `json:"provider"`
	Model      string     `json:"model"`
	Turns      int        `json:"turns"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ClosedAt   *time.Time `json:"closed_at,omitempty"`
	Bytes      int64      `json:"bytes"`
	Loadable   bool       `json:"loadable"`
	Diagnostic string     `json:"diagnostic,omitempty"`
}

type AttachmentRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}
