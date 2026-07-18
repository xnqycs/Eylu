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
}
