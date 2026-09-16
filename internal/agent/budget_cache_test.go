package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

// A cached input token is already inside the input tokens of the same call, so it
// is reported without changing what the request is charged for.
func TestCacheHitsAreReportedWithoutChangingTheBudget(t *testing.T) {
	tracker := newBudgetTracker(150)
	tracker.add(callMain, protocol.Usage{InputTokens: 100, OutputTokens: 50, CachedInputTokens: 80, Exact: true})
	if tracker.report().Total() != 150 {
		t.Fatalf("total = %d, want the input plus output only", tracker.report().Total())
	}
	if tracker.report().CachedInputTokens != 80 {
		t.Fatalf("cached = %d", tracker.report().CachedInputTokens)
	}
	// The threshold semantics are unchanged by the cache figure: spending exactly
	// the limit is still allowed and one token more is not.
	if tracker.exceeded() {
		t.Fatal("a cache hit changed the amount spent")
	}
	tracker.add(callMain, protocol.Usage{InputTokens: 0, OutputTokens: 1, CachedInputTokens: 0, Exact: true})
	if !tracker.exceeded() {
		t.Fatal("the boundary moved once cache tokens were reported")
	}
}

// A budget smaller than the estimated input refuses the request before the model
// is called, which is the consequence the README states.
func TestBudgetSmallerThanThePromptIsRefusedBeforeTheModelCall(t *testing.T) {
	calls := 0
	model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		calls++
		return textResponse("agent-1", "done"), nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	report := RunReport{}
	runtime := testRuntime(model, 1)
	runtime.OutputReserveTokens = 100
	_, err := NewConversation().Run(context.Background(), "a request whose prompt is larger than the whole budget allows", runtime, executor,
		LoopOptions{MaxTurns: 2, MaxTotalTokens: 1, Report: &report}, false, nil)
	if err == nil {
		t.Fatal("a request that cannot fit in its budget was started anyway")
	}
	var protocolErr *protocol.Error
	if !errors.As(err, &protocolErr) || protocolErr.ContextLimit != 1 {
		t.Fatalf("err = %v, want the budget error to name the limit", err)
	}
	if calls != 0 {
		t.Fatalf("the model was called %d times for a request that could not fit", calls)
	}
	if report.StopReason != "token_budget" {
		t.Fatalf("stop reason = %q, want token_budget", report.StopReason)
	}
}

// The cache figure accumulates across the calls of one request, like the rest of
// the usage, so cost accounting sees the whole request rather than the last call.
func TestRunAccumulatesCachedInputTokens(t *testing.T) {
	usage := RunUsage{}
	calls := 0
	model := &funcDriver{name: "loop", generate: func(int, driver.Request) (protocol.ModelResponse, error) {
		calls++
		if calls == 1 {
			return toolUseResponse("agent-1", protocol.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)}), nil
		}
		return textResponse("agent-2", "done"), nil
	}}
	executor := &tool.Executor{Registry: tool.NewRegistry(&countingEchoTool{}), Policy: policy.AllowAllChecker{}}
	// The driver wrapper stands in for a provider that reports cache hits.
	faults := cacheReportingDriver{Driver: model, cached: 4}
	report := RunReport{}
	if _, err := NewConversation().Run(context.Background(), "cache", testRuntime(&faults, 1), executor,
		LoopOptions{MaxTurns: 3, MaxTotalTokens: 1_000_000, Usage: &usage, Report: &report}, false, nil); err != nil {
		t.Fatalf("err = %v", err)
	}
	if usage.CachedInputTokens != 8 || report.CachedInputTokens != 8 {
		t.Fatalf("cached = %d report = %d, want the sum of both calls", usage.CachedInputTokens, report.CachedInputTokens)
	}
	if usage.InputTokens != report.InputTokens || usage.CachedInputTokens > usage.InputTokens {
		t.Fatalf("usage = %#v report = %#v", usage, report)
	}
}

// cacheReportingDriver adds a cache figure to whatever the wrapped driver
// returned, which is what a provider reporting a cache hit does.
type cacheReportingDriver struct {
	Driver driver.ModelDriver
	cached int
}

func (d *cacheReportingDriver) Name() string { return d.Driver.Name() }
func (d *cacheReportingDriver) Capabilities() driver.Capabilities {
	return d.Driver.Capabilities()
}

func (d *cacheReportingDriver) Generate(ctx context.Context, request driver.Request, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	response, err := d.Driver.Generate(ctx, request, emit)
	if err != nil {
		return response, err
	}
	response.Usage.CachedInputTokens = d.cached
	return response, nil
}
