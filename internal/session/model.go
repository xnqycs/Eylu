package session

import (
	"encoding/json"
	"time"

	contextledger "Eylu/internal/context"
	"Eylu/internal/environment"
	"Eylu/internal/protocol"
)

const SchemaVersion = 1

type EventType string

const (
	EventSessionCreated  EventType = "session_created"
	EventTurnAppended    EventType = "turn_appended"
	EventRuntimeUpdated  EventType = "runtime_updated"
	EventDriverState     EventType = "driver_state_updated"
	EventSkillActivated  EventType = "skill_activated"
	EventContextUpdated  EventType = "context_updated"
	EventErrorRecorded   EventType = "error_recorded"
	EventSessionClosed   EventType = "session_closed"
	EventSessionReopened EventType = "session_reopened"
)

type ProviderState struct {
	Name          string `json:"name"`
	Generation    uint64 `json:"generation"`
	Adapter       string `json:"adapter"`
	BaseURL       string `json:"base_url"`
	Model         string `json:"model"`
	ContextWindow int    `json:"context_window,omitempty"`
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
	DriverState    json.RawMessage           `json:"driver_state,omitempty"`
	SkillCatalog   string                    `json:"skill_catalog,omitempty"`
	Skills         []SkillState              `json:"skills,omitempty"`
	Summary        string                    `json:"summary,omitempty"`
	OmittedTurnIDs []string                  `json:"omitted_turn_ids,omitempty"`
	Ledger         contextledger.LedgerState `json:"ledger"`
	LastError      string                    `json:"last_error,omitempty"`
}

type Event struct {
	Version        int                        `json:"version"`
	Sequence       uint64                     `json:"sequence"`
	Type           EventType                  `json:"type"`
	SessionID      string                     `json:"session_id"`
	At             time.Time                  `json:"at"`
	Workspace      string                     `json:"workspace,omitempty"`
	Environment    *environment.Context       `json:"environment,omitempty"`
	PermissionMode string                     `json:"permission_mode,omitempty"`
	Provider       *ProviderState             `json:"provider,omitempty"`
	Turn           *protocol.Turn             `json:"turn,omitempty"`
	DriverState    json.RawMessage            `json:"driver_state,omitempty"`
	Skill          *SkillState                `json:"skill,omitempty"`
	SkillCatalog   string                     `json:"skill_catalog,omitempty"`
	Summary        string                     `json:"summary,omitempty"`
	OmittedTurnIDs []string                   `json:"omitted_turn_ids,omitempty"`
	Ledger         *contextledger.LedgerState `json:"ledger,omitempty"`
	Error          string                     `json:"error,omitempty"`
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
