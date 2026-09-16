package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/tool"
)

// heldAppDriver holds one request open until the test releases it, so a settings
// change can be delivered while the request is provably running.
type heldAppDriver struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newHeldAppDriver() *heldAppDriver {
	return &heldAppDriver{started: make(chan struct{}), release: make(chan struct{})}
}

func (*heldAppDriver) Name() string { return "held-app" }
func (*heldAppDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{}
}

func (d *heldAppDriver) Generate(ctx context.Context, _ driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.once.Do(func() { close(d.started) })
	select {
	case <-d.release:
		return protocol.ModelResponse{
			Turn: protocol.Turn{ID: "agent-1", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "done"}}},
			Stop: protocol.StopCompleted,
		}, nil
	case <-ctx.Done():
		return protocol.ModelResponse{}, ctx.Err()
	}
}

// startHeldRequest runs one request on a backend whose request can be held open.
func startHeldRequest(t *testing.T, mode string) (*tuiBackend, *heldAppDriver, chan error, *agent.RunReport) {
	t.Helper()
	cfg := config.Default()
	cfg.PermissionMode = mode
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "held-app", BaseURL: "https://example.test/v1", Model: "model"}
	manager, err := provider.NewManager(t.TempDir()+"/config.toml", cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	model := newHeldAppDriver()
	conversation := agent.NewConversation()
	backend := &tuiBackend{runtime: &runtime{}, conversation: conversation, manager: manager, opts: chatOptions{mode: mode}}
	report := &agent.RunReport{}
	modelRuntime := agent.Runtime{
		Provider:       provider.Snapshot{Name: "work", Generation: 1, Config: cfg.Providers["work"]},
		Driver:         model,
		PermissionMode: mode,
		Workspace:      t.TempDir(),
		Timeout:        time.Second,
	}
	executor := &tool.Executor{Registry: tool.NewRegistry(), Policy: policy.AllowAllChecker{}}
	done := make(chan error, 1)
	go func() {
		_, err := conversation.Run(context.Background(), "hold", modelRuntime, executor,
			agent.LoopOptions{MaxTurns: 1, MaxTotalTokens: 1_000_000, Report: report}, false, nil)
		done <- err
	}()
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never started")
	}
	return backend, model, done, report
}

// A mode that narrows while a request is running reaches that request, and the
// outcome says it stopped on the new settings rather than reporting a bare
// cancellation or a model failure.
func TestSetModeStopsTheRunningRequestWhenItNarrows(t *testing.T) {
	backend, _, done, report := startHeldRequest(t, "full")
	backend.beginSafetyBaseline()
	defer backend.endSafetyBaseline()

	if err := backend.SetMode(context.Background(), "plan"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var stopErr *agent.PolicyStopError
		if !errors.As(err, &stopErr) {
			t.Fatalf("err = %v, want a policy stop", err)
		}
		if stopErr.Reason != "permission mode full -> plan" {
			t.Fatalf("reason = %q", stopErr.Reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request kept running after the mode narrowed")
	}
	if report.StopReason != "policy_tightened" {
		t.Fatalf("stop reason = %q", report.StopReason)
	}
}

// A mode that widens belongs to the next request: the one that is running keeps
// going and finishes normally.
func TestSetModeWideningLeavesTheRunningRequestAlone(t *testing.T) {
	backend, model, done, report := startHeldRequest(t, "plan")
	backend.beginSafetyBaseline()
	defer backend.endSafetyBaseline()

	if err := backend.SetMode(context.Background(), "full"); err != nil {
		t.Fatal(err)
	}
	close(model.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the widened request failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not finish")
	}
	if report.StopReason != string(protocol.StopCompleted) {
		t.Fatalf("stop reason = %q, want the request to have completed", report.StopReason)
	}
}

// Nothing is stopped when no request is running, so a change made between
// requests is applied by the next request being built.
func TestTighteningBetweenRequestsStopsNothing(t *testing.T) {
	cfg := config.Default()
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "held-app", BaseURL: "https://example.test/v1", Model: "model"}
	manager, err := provider.NewManager(t.TempDir()+"/config.toml", cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	backend := &tuiBackend{runtime: &runtime{}, conversation: agent.NewConversation(), manager: manager, opts: chatOptions{mode: "full"}}
	backend.beginSafetyBaseline()
	defer backend.endSafetyBaseline()
	if reason := backend.stopIfTightened(); reason != "" {
		t.Fatalf("an idle backend reported a stop: %q", reason)
	}
	backend.mu.Lock()
	backend.opts.mode = "manual"
	backend.mu.Unlock()
	if reason := backend.stopIfTightened(); reason != "" {
		t.Fatalf("an idle backend stopped a request that was not running: %q", reason)
	}
}
