package metrics

import (
	"testing"
	"time"

	contextledger "Eylu/internal/context"
	"Eylu/internal/protocol"
)

func TestObservationAndSummary(t *testing.T) {
	collector := &Collector{}
	observation := collector.Begin(Metadata{
		RequestID: "request", SessionID: "session", Provider: "provider", ProviderGeneration: 3, Model: "model",
		InputCostPerMillion: 2, OutputCostPerMillion: 4,
	})
	time.Sleep(time.Millisecond)
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: "x"})
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventToolStart})
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventToolResult, ToolResult: &protocol.ToolResult{}})
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventUsage, Usage: &protocol.Usage{InputTokens: 100, OutputTokens: 50, Exact: true}})
	observation.ObserveContextEvent(contextledger.Event{Kind: contextledger.EventCompression})
	metric := observation.Finish(protocol.Usage{}, nil)
	if metric.RequestID != "request" || metric.FirstTokenMS < 1 || metric.ToolSuccessRate != 1 || metric.CompressionCount != 1 || metric.EstimatedCost != 0.0004 {
		t.Fatalf("metric = %#v", metric)
	}
	summary := collector.Snapshot()
	if summary.Requests != 1 || summary.ToolCalls != 1 || summary.ToolSuccessRate != 1 || summary.Usage.InputTokens != 100 || summary.EstimatedCost != metric.EstimatedCost {
		t.Fatalf("summary = %#v", summary)
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
	observation := (&Collector{}).Begin(Metadata{})
	time.Sleep(time.Millisecond)
	observation.ObserveModelEvent(protocol.ModelEvent{Kind: protocol.EventToolCallDelta, ToolCallDelta: &protocol.ToolCallDelta{Name: "write_file"}})
	metric := observation.Finish(protocol.Usage{}, nil)
	if metric.FirstTokenMS < 1 {
		t.Fatalf("metric = %#v", metric)
	}
}
