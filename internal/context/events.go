package context

import (
	"time"

	"Eylu/internal/protocol"
)

type EventKind string

const (
	EventBudget             EventKind = "budget"
	EventCompressionStarted EventKind = "compression_started"
	EventCompression        EventKind = "compression"
	EventCompressionFailed  EventKind = "compression_failed"
)

type Event struct {
	Kind          EventKind         `json:"kind"`
	InputTokens   int               `json:"input_tokens"`
	OutputReserve int               `json:"output_reserve"`
	ContextWindow int               `json:"context_window,omitempty"`
	Percent       float64           `json:"percent,omitempty"`
	Compression   *CompressionEvent `json:"compression,omitempty"`
	Error         string            `json:"error,omitempty"`
}

type CompressionEvent struct {
	Trigger      string         `json:"trigger,omitempty"`
	Strategy     string         `json:"strategy,omitempty"`
	BeforeTokens int            `json:"before_tokens"`
	AfterTokens  int            `json:"after_tokens"`
	OmittedTurns int            `json:"omitted_turns"`
	SummaryBytes int            `json:"summary_bytes"`
	DurationMS   int64          `json:"duration_ms,omitempty"`
	Usage        protocol.Usage `json:"usage,omitzero"`
	Noop         bool           `json:"noop,omitempty"`
	OccurredAt   time.Time      `json:"occurred_at"`
}
