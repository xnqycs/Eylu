package metrics

import (
	"math"
	"testing"
	"time"

	contextledger "Eylu/internal/context"
	"Eylu/internal/protocol"
)

func TestObservationAndSummary(t *testing.T) {
	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	collector := &Collector{now: func() time.Time { return now }}
	observation := collector.Begin(Metadata{
		RequestID: "request", SessionID: "session", Provider: "provider", ProviderGeneration: 3, Model: "model",
		InputCostPerMillion: 2, OutputCostPerMillion: 4,
	})
	now = now.Add(time.Second)
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventResponseStart})
	now = now.Add(2 * time.Second)
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: "x"})
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventToolStart})
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventToolResult, ToolResult: &protocol.ToolResult{}})
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventWebSearchStarted, WebActivity: &protocol.WebActivity{CallID: "web-1", Kind: protocol.ToolWebSearch, Status: protocol.WebStatusRunning}})
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventWebSearchCompleted, WebActivity: &protocol.WebActivity{CallID: "web-1", Kind: protocol.ToolWebSearch, Status: protocol.WebStatusCompleted, Usage: protocol.WebUsage{Searches: 1, InputTokens: 7, OutputTokens: 3, CostUSD: 0.01}}})
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventCitation, Citation: &protocol.URLCitation{CallID: "web-1", URL: "https://example.com"}})
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventUsage, Usage: &protocol.Usage{InputTokens: 100, OutputTokens: 50, Exact: true}})
	now = now.Add(2 * time.Second)
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventResponseDone})
	observation.ObserveContextEvent(contextledger.Event{Kind: contextledger.EventCompression, Compression: &contextledger.CompressionEvent{DurationMS: 250, Strategy: "deterministic_fallback", Usage: protocol.Usage{InputTokens: 20, OutputTokens: 5, Exact: true}}})
	metric := observation.Finish(protocol.Usage{}, nil)
	if metric.RequestID != "request" || metric.FirstTokenMS != 3000 || metric.GenerationMS != 2000 || metric.TokensPerSecond != 25 || metric.ToolSuccessRate != 1 || metric.CompressionCount != 1 || metric.CompactionDurationMS != 250 || metric.CompactionFallbacks != 1 || metric.CompactionUsage.OutputTokens != 5 || metric.EstimatedCost != 0.0004 || math.Abs(metric.CompactionCost-0.00006) > 1e-12 || metric.WebActivities != 1 || metric.WebCitations != 1 || metric.WebUsage.Searches != 1 || metric.WebUsage.CostUSD != 0.01 {
		t.Fatalf("metric = %#v", metric)
	}
	summary := collector.Snapshot()
	if summary.Requests != 1 || summary.ToolCalls != 1 || summary.ToolSuccessRate != 1 || summary.Usage.InputTokens != 100 || summary.EstimatedCost != metric.EstimatedCost || summary.WebActivities != 1 || summary.WebCitations != 1 || summary.WebUsage.Searches != 1 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestObservationAggregatesGenerationAcrossToolRounds(t *testing.T) {
	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	observation := (&Collector{now: func() time.Time { return now }}).Begin(Metadata{})
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventResponseStart})
	now = now.Add(3 * time.Second)
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventToolCallDelta, ToolCallDelta: &protocol.ToolCallDelta{Name: "read_file"}})
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventUsage, Usage: &protocol.Usage{OutputTokens: 30, Exact: true}})
	now = now.Add(2 * time.Second)
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventResponseDone})
	now = now.Add(5 * time.Second) // tool execution is outside generation time
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventResponseStart})
	now = now.Add(2 * time.Second)
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: "done"})
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventUsage, Usage: &protocol.Usage{OutputTokens: 50, Exact: true}})
	now = now.Add(2 * time.Second)
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventResponseDone})
	metric := observation.Finish(protocol.Usage{}, nil)
	if metric.FirstTokenMS != 3000 || metric.GenerationMS != 4000 || metric.TokensPerSecond != 20 || metric.DurationMS != 14000 {
		t.Fatalf("metric = %#v", metric)
	}
}

func TestObservationRecordsErrorAndFallbackUsage(t *testing.T) {
	collector := &Collector{}
	metric := collector.Begin(Metadata{}).Finish(protocol.Usage{InputTokens: 5}, &protocol.Error{Code: protocol.ErrRateLimit})
	if metric.ErrorCode != string(protocol.ErrRateLimit) || metric.Usage.InputTokens != 5 || collector.Snapshot().Failures != 1 {
		t.Fatalf("metric = %#v", metric)
	}
}

func TestToolCallDeltaMarksFirstToken(t *testing.T) {
	for _, event := range []protocol.ModelEvent{
		{Kind: protocol.EventToolCallDelta, ToolCallDelta: &protocol.ToolCallDelta{Name: "write_file"}},
		{Kind: protocol.EventReasoningDelta, Delta: "thinking"},
	} {
		observation := (&Collector{}).Begin(Metadata{})
		time.Sleep(time.Millisecond)
		observation.ObserveModelEvent(event)
		metric := observation.Finish(protocol.Usage{}, nil)
		if metric.FirstTokenMS < 1 {
			t.Fatalf("event=%s metric=%#v", event.Kind, metric)
		}
	}
}
