package context

import "time"

type EventKind string

const (
	EventBudget      EventKind = "budget"
	EventCompression EventKind = "compression"
)

type Event struct {
	Kind          EventKind         `json:"kind"`
	InputTokens   int               `json:"input_tokens"`
	OutputReserve int               `json:"output_reserve"`
	ContextWindow int               `json:"context_window,omitempty"`
	Percent       float64           `json:"percent,omitempty"`
	Compression   *CompressionEvent `json:"compression,omitempty"`
}

type CompressionEvent struct {
	BeforeTokens int       `json:"before_tokens"`
	AfterTokens  int       `json:"after_tokens"`
	OmittedTurns int       `json:"omitted_turns"`
	SummaryBytes int       `json:"summary_bytes"`
	OccurredAt   time.Time `json:"occurred_at"`
}
