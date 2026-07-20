package context

import (
	"bytes"
	"strings"
	"testing"

	"Eylu/internal/protocol"
)

func TestLedgerReportTotalsAndUnknownWindow(t *testing.T) {
	ledger := New(ApproxEstimator{BytesPerToken: 2})
	ledger.AddText("system", CategorySystemPrompt, "test", "1234", true)
	ledger.AddText("user", CategoryUserMessage, "turn", "123456", false)
	ledger.Add(Block{ID: "reserve", Category: CategoryOutputReserve, Tokens: 100})
	ledger.SetLastUsage(protocol.Usage{InputTokens: 9, OutputTokens: 3, Exact: true})
	report := ledger.Report("work", "model", 0)
	if report.InputTokens != 5 || report.OutputReserve != 100 || report.LimitSource != "unknown" || report.Percent != 0 {
		t.Fatalf("report = %#v", report)
	}
	if len(report.Categories) != len(categoryOrder) {
		t.Fatalf("categories = %d", len(report.Categories))
	}
	var output bytes.Buffer
	if err := RenderText(&output, report); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Context  5 input + 100 reserved / unknown", "System prompt", "MCP resources", "Last provider usage"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("render missing %q:\n%s", expected, output.String())
		}
	}
}

func TestLedgerKnownWindowPercentage(t *testing.T) {
	ledger := New(ApproxEstimator{BytesPerToken: 1})
	ledger.AddText("user", CategoryUserMessage, "turn", strings.Repeat("x", 50), false)
	ledger.Add(Block{ID: "reserve", Category: CategoryOutputReserve, Tokens: 50})
	report := ledger.Report("work", "model", 200)
	if report.Percent != 50 || report.LimitSource != "provider_config" {
		t.Fatalf("report = %#v", report)
	}
	var userMessages CategoryUsage
	for _, category := range report.Categories {
		if category.Category == CategoryUserMessage {
			userMessages = category
			break
		}
	}
	if !report.LimitKnown || report.TotalTokens != 100 || userMessages.Measurement != "estimated" {
		t.Fatalf("stable report fields = %#v", report)
	}
}

func TestLedgerReportsConfiguredDetectedAndEffectiveLimits(t *testing.T) {
	ledger := New(ApproxEstimator{BytesPerToken: 1})
	ledger.ReplaceBlocks([]Block{{ID: "input", Category: CategoryUserMessage, Bytes: 100, Tokens: 100}})
	report := ledger.ReportWithLimits("work", "model", LimitDetails{Configured: 64000, Detected: 128000, Effective: 64000, Source: "user_cap", Cached: true, Degradations: 2})
	if report.ConfiguredContextWindow != 64000 || report.DetectedContextWindow != 128000 || report.ContextWindow != 64000 || report.LimitSource != "user_cap" || !report.LimitCached || report.LimitDegradations != 2 {
		t.Fatalf("report = %#v", report)
	}
	var output bytes.Buffer
	if err := RenderText(&output, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Configured 64000 · detected 128000 · effective 64000") {
		t.Fatalf("render = %s", output.String())
	}
}

func TestLedgerSourceBreakdownAndCompression(t *testing.T) {
	ledger := New(ApproxEstimator{BytesPerToken: 1})
	ledger.AddText("catalog-1", CategorySkillCatalog, "page:1/2", "abc", true)
	ledger.AddText("catalog-2", CategorySkillCatalog, "page:2/2", "de", true)
	event := CompressionEvent{BeforeTokens: 20, AfterTokens: 10, OmittedTurns: 4, SummaryBytes: 30}
	ledger.RecordCompression(event)
	report := ledger.Report("work", "model", 100)
	var catalog CategoryUsage
	for _, category := range report.Categories {
		if category.Category == CategorySkillCatalog {
			catalog = category
		}
	}
	if catalog.Tokens != 5 || len(catalog.Sources) != 2 || catalog.Sources[0].Source != "page:1/2" || report.CompressionCount != 1 || report.LastCompression.OmittedTurns != 4 {
		t.Fatalf("report = %#v", report)
	}
}

func TestLedgerUnknownCategoryContributesToTotals(t *testing.T) {
	ledger := New(ApproxEstimator{BytesPerToken: 1})
	ledger.AddText("future", Category("future_context"), "extension", "12345", false)
	report := ledger.Report("work", "model", 100)
	if report.InputTokens != 5 || report.Categories[len(report.Categories)-1].Category != "future_context" || report.Categories[len(report.Categories)-1].Measurement != "estimated" {
		t.Fatalf("report = %#v", report)
	}
}

func TestLedgerStateRoundTrip(t *testing.T) {
	ledger := New(ApproxEstimator{BytesPerToken: 1})
	ledger.AddText("user", CategoryUserMessage, "turn", "hello", false)
	ledger.SetLastUsage(protocol.Usage{InputTokens: 5, OutputTokens: 1, Exact: true})
	ledger.RecordCompression(CompressionEvent{Trigger: "manual", Strategy: "model", BeforeTokens: 10, AfterTokens: 5, OmittedTurns: 2, DurationMS: 25, Usage: protocol.Usage{InputTokens: 3, OutputTokens: 1, Exact: true}})
	restored := New(nil)
	restored.Restore(ledger.State())
	report := restored.Report("provider", "model", 100)
	if report.InputTokens != 5 || report.LastUsage.InputTokens != 5 || report.CompressionCount != 1 || report.LastCompression.OmittedTurns != 2 || report.LastCompression.Trigger != "manual" || report.LastCompression.Strategy != "model" || report.LastCompression.DurationMS != 25 || report.LastCompression.Usage.OutputTokens != 1 {
		t.Fatalf("report = %#v", report)
	}
}

func TestLedgerStateDoesNotShareMetadataMap(t *testing.T) {
	ledger := New(nil)
	ledger.ReplaceBlocks([]Block{{ID: "block", Category: CategoryUserMessage, Metadata: map[string]any{"key": "saved"}}})
	state := ledger.State()
	state.Blocks[0].Metadata["key"] = "changed"
	if value := ledger.Blocks()[0].Metadata["key"]; value != "saved" {
		t.Fatalf("metadata = %v", value)
	}
}
