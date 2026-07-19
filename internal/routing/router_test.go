package routing

import (
	"testing"

	"Eylu/internal/config"
	"Eylu/internal/driver"
	"Eylu/internal/provider"
)

func TestSelectFiltersCapabilitiesContextAndTasks(t *testing.T) {
	providers := []provider.Snapshot{
		providerSnapshot("cheap-review", "chat", 64_000, config.ProviderRouting{Tasks: []string{TaskReview}, InputCostPerMillion: 0.1}),
		providerSnapshot("capable-review", "responses", 128_000, config.ProviderRouting{Tasks: []string{TaskReview}, InputCostPerMillion: 2}),
		providerSnapshot("small-review", "responses", 4_000, config.ProviderRouting{Tasks: []string{TaskReview}, Priority: 100}),
		providerSnapshot("coding", "responses", 128_000, config.ProviderRouting{Tasks: []string{TaskCoding}}),
	}
	decision, err := Select(providers, Request{
		Task: TaskReview, RequiredContext: 8_000, EstimatedInput: 10_000,
		Capabilities: driver.Capabilities{ToolCalling: true, Reasoning: true},
	}, testCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Provider != "capable-review" || len(decision.Candidates) != 1 {
		t.Fatalf("decision = %#v", decision)
	}
	if decision.Rejected["cheap-review"] == "" || decision.Rejected["small-review"] == "" || decision.Rejected["coding"] == "" {
		t.Fatalf("rejections = %#v", decision.Rejected)
	}
}

func TestSelectUsesPriorityCostContextAndNameDeterministically(t *testing.T) {
	providers := []provider.Snapshot{
		providerSnapshot("z-expensive", "responses", 64_000, config.ProviderRouting{Tasks: []string{TaskGeneral}, Priority: 2, InputCostPerMillion: 10}),
		providerSnapshot("b-cheap", "responses", 64_000, config.ProviderRouting{Tasks: []string{TaskGeneral}, Priority: 1, InputCostPerMillion: 1}),
		providerSnapshot("a-cheap", "responses", 128_000, config.ProviderRouting{Tasks: []string{TaskGeneral}, Priority: 1, InputCostPerMillion: 1}),
	}
	decision, err := Select(providers, Request{Task: TaskCoding, EstimatedInput: 1000}, testCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Provider != "z-expensive" {
		t.Fatalf("priority decision = %#v", decision.Candidates)
	}
	providers[0].Config.Routing.Priority = 1
	decision, err = Select(providers, Request{Task: TaskCoding, EstimatedInput: 1000}, testCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Provider != "a-cheap" {
		t.Fatalf("cost/context decision = %#v", decision.Candidates)
	}
	providers[2].Config.ContextWindow = 64_000
	decision, _ = Select(providers, Request{Task: TaskCoding, EstimatedInput: 1000}, testCapabilities)
	if decision.Provider != "a-cheap" {
		t.Fatalf("name tie-break decision = %#v", decision.Candidates)
	}
}

func TestClassify(t *testing.T) {
	for prompt, expected := range map[string]string{
		"review this diff": TaskReview, "修复这个报错": TaskDebugging, "increase test coverage": TaskTesting,
		"update README": TaskDocumentation, "实现新功能": TaskCoding, "tell me something": TaskGeneral,
	} {
		if actual := Classify(prompt); actual != expected {
			t.Fatalf("Classify(%q) = %q, want %q", prompt, actual, expected)
		}
	}
}

func providerSnapshot(name, adapter string, contextWindow int, routing config.ProviderRouting) provider.Snapshot {
	return provider.Snapshot{Name: name, Generation: 1, Config: config.ProviderConfig{Adapter: adapter, ContextWindow: contextWindow, Routing: routing}}
}

func testCapabilities(adapter string) (driver.Capabilities, bool) {
	switch adapter {
	case "responses":
		return driver.Capabilities{TextStreaming: true, ToolCalling: true, ParallelTools: true, Reasoning: true}, true
	case "chat":
		return driver.Capabilities{TextStreaming: true, ToolCalling: true}, true
	default:
		return driver.Capabilities{}, false
	}
}
