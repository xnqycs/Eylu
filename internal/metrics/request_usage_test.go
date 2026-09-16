package metrics

import (
	"testing"

	"Eylu/internal/protocol"
)

// Metrics carry both usage figures, and the summary accumulates the request totals
// rather than the last calls: the two answer different questions and the summary is
// read for cost, which is the request.
func TestCollectorAccumulatesRequestUsageApartFromTheLastCall(t *testing.T) {
	collector := &Collector{}
	collector.add(RequestMetric{
		RequestID: "one", Usage: protocol.Usage{InputTokens: 100, OutputTokens: 10},
		RequestUsage:      protocol.Usage{InputTokens: 100, OutputTokens: 10, CachedInputTokens: 60, Exact: true},
		RequestModelCalls: 1,
	})
	collector.add(RequestMetric{
		RequestID: "two", Usage: protocol.Usage{InputTokens: 100, OutputTokens: 10},
		RequestUsage: protocol.Usage{InputTokens: 100, OutputTokens: 10}, RequestModelCalls: 1,
	})
	summary := collector.Snapshot()
	if summary.Usage.InputTokens != 200 || summary.Usage.OutputTokens != 20 {
		t.Fatalf("the last-call summary = %#v", summary.Usage)
	}
	if summary.RequestUsage.InputTokens != 200 || summary.RequestUsage.OutputTokens != 20 {
		t.Fatalf("the request summary = %#v", summary.RequestUsage)
	}
	if summary.RequestUsage.CachedInputTokens != 60 {
		t.Fatalf("cache hits were lost in the summary: %#v", summary.RequestUsage)
	}
	if summary.RequestUsage.Exact == false {
		t.Fatal("the exactness of the request totals was lost")
	}
	if summary.RequestModelCalls != 2 || summary.Requests != 2 {
		t.Fatalf("model calls = %d requests = %d", summary.RequestModelCalls, summary.Requests)
	}
}
