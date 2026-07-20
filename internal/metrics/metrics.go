package metrics

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	contextledger "Eylu/internal/context"
	"Eylu/internal/protocol"
)

type Metadata struct {
	RequestID            string  `json:"request_id"`
	SessionID            string  `json:"session_id"`
	Provider             string  `json:"provider_name"`
	ProviderGeneration   uint64  `json:"provider_generation"`
	Model                string  `json:"model"`
	Task                 string  `json:"task,omitempty"`
	InputCostPerMillion  float64 `json:"-"`
	OutputCostPerMillion float64 `json:"-"`
}

type RequestMetric struct {
	Timestamp            time.Time      `json:"timestamp"`
	RequestID            string         `json:"request_id"`
	SessionID            string         `json:"session_id"`
	Provider             string         `json:"provider_name"`
	ProviderGeneration   uint64         `json:"provider_generation"`
	Model                string         `json:"model"`
	Task                 string         `json:"task,omitempty"`
	FirstTokenMS         int64          `json:"first_token_ms,omitempty"`
	GenerationMS         int64          `json:"generation_ms,omitempty"`
	TokensPerSecond      float64        `json:"tokens_per_second,omitempty"`
	DurationMS           int64          `json:"duration_ms"`
	ToolCalls            int            `json:"tool_calls"`
	ToolSuccesses        int            `json:"tool_successes"`
	ToolSuccessRate      float64        `json:"tool_success_rate"`
	CompressionCount     int            `json:"compression_count"`
	CompactionDurationMS int64          `json:"compaction_duration_ms,omitempty"`
	CompactionFallbacks  int            `json:"compaction_fallbacks,omitempty"`
	CompactionUsage      protocol.Usage `json:"compaction_usage,omitzero"`
	CompactionCost       float64        `json:"compaction_estimated_cost,omitempty"`
	Usage                protocol.Usage `json:"usage"`
	EstimatedCost        float64        `json:"estimated_cost"`
	ErrorCode            string         `json:"error_code,omitempty"`
}

type Summary struct {
	Requests             int            `json:"requests"`
	Failures             int            `json:"failures"`
	AverageFirstTokenMS  float64        `json:"average_first_token_ms"`
	AverageDurationMS    float64        `json:"average_duration_ms"`
	ToolCalls            int            `json:"tool_calls"`
	ToolSuccesses        int            `json:"tool_successes"`
	ToolSuccessRate      float64        `json:"tool_success_rate"`
	CompressionCount     int            `json:"compression_count"`
	CompactionDurationMS int64          `json:"compaction_duration_ms,omitempty"`
	CompactionFallbacks  int            `json:"compaction_fallbacks,omitempty"`
	CompactionUsage      protocol.Usage `json:"compaction_usage,omitzero"`
	CompactionCost       float64        `json:"compaction_estimated_cost,omitempty"`
	Usage                protocol.Usage `json:"usage"`
	EstimatedCost        float64        `json:"estimated_cost"`
}

type Collector struct {
	mu      sync.Mutex
	records []RequestMetric
	now     func() time.Time
}

type Observation struct {
	mu                  sync.Mutex
	collector           *Collector
	metadata            Metadata
	started             time.Time
	firstToken          time.Time
	roundFirst          time.Time
	generation          time.Duration
	toolCalls           int
	toolSuccesses       int
	compressions        int
	compactionDuration  time.Duration
	compactionFallbacks int
	compactionUsage     protocol.Usage
	usage               protocol.Usage
	finished            bool
	now                 func() time.Time
}

func (c *Collector) Begin(metadata Metadata) *Observation {
	if metadata.RequestID == "" {
		metadata.RequestID = uuid.NewString()
	}
	now := time.Now
	if c != nil && c.now != nil {
		now = c.now
	}
	return &Observation{collector: c, metadata: metadata, started: now(), now: now}
}

func (o *Observation) RequestID() string { return o.metadata.RequestID }

func (o *Observation) ObserveModelEvent(event protocol.ModelEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch event.Kind {
	case protocol.EventResponseStart:
		o.closeGenerationRound(o.now())
	case protocol.EventTextDelta, protocol.EventReasoningDelta:
		if event.Delta != "" {
			o.observeOutput(o.now())
		}
	case protocol.EventToolCallDelta:
		if event.ToolCallDelta != nil {
			o.observeOutput(o.now())
		}
	case protocol.EventToolStart:
		o.toolCalls++
	case protocol.EventToolResult:
		if event.ToolResult != nil && !event.ToolResult.IsError {
			o.toolSuccesses++
		}
	case protocol.EventUsage:
		if event.Usage != nil {
			o.usage.InputTokens += event.Usage.InputTokens
			o.usage.OutputTokens += event.Usage.OutputTokens
			o.usage.ReasoningTokens += event.Usage.ReasoningTokens
			o.usage.Exact = o.usage.Exact || event.Usage.Exact
		}
	case protocol.EventResponseDone:
		o.closeGenerationRound(o.now())
	}
}

func (o *Observation) observeOutput(now time.Time) {
	if o.firstToken.IsZero() {
		o.firstToken = now
	}
	if o.roundFirst.IsZero() {
		o.roundFirst = now
	}
}

func (o *Observation) closeGenerationRound(now time.Time) {
	if !o.roundFirst.IsZero() && now.After(o.roundFirst) {
		o.generation += now.Sub(o.roundFirst)
	}
	o.roundFirst = time.Time{}
}

func (o *Observation) ObserveContextEvent(event contextledger.Event) {
	if event.Kind != contextledger.EventCompression || event.Compression == nil {
		return
	}
	o.mu.Lock()
	o.compressions++
	o.compactionDuration += time.Duration(event.Compression.DurationMS) * time.Millisecond
	if event.Compression.Strategy == "deterministic_fallback" {
		o.compactionFallbacks++
	}
	o.compactionUsage.InputTokens += event.Compression.Usage.InputTokens
	o.compactionUsage.OutputTokens += event.Compression.Usage.OutputTokens
	o.compactionUsage.ReasoningTokens += event.Compression.Usage.ReasoningTokens
	o.compactionUsage.Exact = o.compactionUsage.Exact || event.Compression.Usage.Exact
	o.mu.Unlock()
}

func (o *Observation) Finish(fallbackUsage protocol.Usage, requestErr error) RequestMetric {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished {
		return RequestMetric{}
	}
	o.finished = true
	if o.usage.InputTokens == 0 && o.usage.OutputTokens == 0 && o.usage.ReasoningTokens == 0 {
		o.usage = fallbackUsage
	}
	now := o.now()
	o.closeGenerationRound(now)
	metric := RequestMetric{
		Timestamp: now.UTC(), RequestID: o.metadata.RequestID, SessionID: o.metadata.SessionID,
		Provider: o.metadata.Provider, ProviderGeneration: o.metadata.ProviderGeneration, Model: o.metadata.Model, Task: o.metadata.Task,
		DurationMS: now.Sub(o.started).Milliseconds(), ToolCalls: o.toolCalls, ToolSuccesses: o.toolSuccesses,
		CompressionCount: o.compressions, CompactionDurationMS: o.compactionDuration.Milliseconds(), CompactionFallbacks: o.compactionFallbacks, CompactionUsage: o.compactionUsage, Usage: o.usage,
	}
	if !o.firstToken.IsZero() {
		metric.FirstTokenMS = o.firstToken.Sub(o.started).Milliseconds()
	}
	metric.GenerationMS = o.generation.Milliseconds()
	if metric.Usage.OutputTokens > 0 && o.generation > 0 {
		metric.TokensPerSecond = float64(metric.Usage.OutputTokens) / o.generation.Seconds()
	}
	if metric.ToolCalls > 0 {
		metric.ToolSuccessRate = float64(metric.ToolSuccesses) / float64(metric.ToolCalls)
	}
	metric.EstimatedCost = float64(metric.Usage.InputTokens)*o.metadata.InputCostPerMillion/1_000_000 +
		float64(metric.Usage.OutputTokens)*o.metadata.OutputCostPerMillion/1_000_000
	metric.CompactionCost = float64(metric.CompactionUsage.InputTokens)*o.metadata.InputCostPerMillion/1_000_000 +
		float64(metric.CompactionUsage.OutputTokens)*o.metadata.OutputCostPerMillion/1_000_000
	if requestErr != nil {
		var typed *protocol.Error
		if errors.As(requestErr, &typed) {
			metric.ErrorCode = string(typed.Code)
		} else {
			metric.ErrorCode = "internal_error"
		}
	}
	if o.collector != nil {
		o.collector.add(metric)
	}
	return metric
}

func (c *Collector) add(metric RequestMetric) {
	c.mu.Lock()
	c.records = append(c.records, metric)
	c.mu.Unlock()
}

func (c *Collector) Snapshot() Summary {
	c.mu.Lock()
	defer c.mu.Unlock()
	var summary Summary
	var firstTokenTotal, firstTokenCount, durationTotal int64
	for _, metric := range c.records {
		summary.Requests++
		if metric.ErrorCode != "" {
			summary.Failures++
		}
		if metric.FirstTokenMS > 0 {
			firstTokenTotal += metric.FirstTokenMS
			firstTokenCount++
		}
		durationTotal += metric.DurationMS
		summary.ToolCalls += metric.ToolCalls
		summary.ToolSuccesses += metric.ToolSuccesses
		summary.CompressionCount += metric.CompressionCount
		summary.CompactionDurationMS += metric.CompactionDurationMS
		summary.CompactionFallbacks += metric.CompactionFallbacks
		summary.CompactionUsage.InputTokens += metric.CompactionUsage.InputTokens
		summary.CompactionUsage.OutputTokens += metric.CompactionUsage.OutputTokens
		summary.CompactionUsage.ReasoningTokens += metric.CompactionUsage.ReasoningTokens
		summary.CompactionUsage.Exact = summary.CompactionUsage.Exact || metric.CompactionUsage.Exact
		summary.CompactionCost += metric.CompactionCost
		summary.Usage.InputTokens += metric.Usage.InputTokens
		summary.Usage.OutputTokens += metric.Usage.OutputTokens
		summary.Usage.ReasoningTokens += metric.Usage.ReasoningTokens
		summary.Usage.Exact = summary.Usage.Exact || metric.Usage.Exact
		summary.EstimatedCost += metric.EstimatedCost
	}
	if firstTokenCount > 0 {
		summary.AverageFirstTokenMS = float64(firstTokenTotal) / float64(firstTokenCount)
	}
	if summary.Requests > 0 {
		summary.AverageDurationMS = float64(durationTotal) / float64(summary.Requests)
	}
	if summary.ToolCalls > 0 {
		summary.ToolSuccessRate = float64(summary.ToolSuccesses) / float64(summary.ToolCalls)
	}
	return summary
}
