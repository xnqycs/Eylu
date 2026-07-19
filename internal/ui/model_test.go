package ui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	contextledger "Eylu/internal/context"
	"Eylu/internal/protocol"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Tick(duration time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { return fn(c.now.Add(duration)) }
}

type fakeBackend struct {
	snapshot Snapshot
	submit   func(context.Context, string, string, func(Event)) error
	err      error
	models   []string
}

func (b *fakeBackend) Snapshot(context.Context) (Snapshot, error) { return b.snapshot, b.err }
func (b *fakeBackend) Submit(ctx context.Context, operationID, prompt string, emit func(Event)) error {
	if b.submit != nil {
		return b.submit(ctx, operationID, prompt, emit)
	}
	return b.err
}
func (b *fakeBackend) Command(context.Context, string) (string, error)    { return "command ok", b.err }
func (b *fakeBackend) UpsertProvider(context.Context, ProviderForm) error { return b.err }
func (b *fakeBackend) DeleteProvider(context.Context, string) error       { return b.err }
func (b *fakeBackend) UseProvider(context.Context, string) error          { return b.err }
func (b *fakeBackend) SetModel(context.Context, string, string) error     { return b.err }
func (b *fakeBackend) FetchModels(context.Context, string) ([]string, error) {
	return append([]string(nil), b.models...), b.err
}

func TestOperationStatesStaleMessagesApprovalAndCancellation(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)}
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Clock: clock, Width: 80, Height: 24})
	model.operationID = "op-current"
	model.startedAt = clock.now.Add(-time.Second)
	for _, state := range []OperationState{StateConnecting, StateFetchingModels, StateWaitingFirstToken, StateStreaming, StateExecutingTool, StateRetryBackoff, StateCancelling, StateCompleted, StateFailed, StateIdle} {
		retry := time.Duration(0)
		if state == StateRetryBackoff {
			retry = 2 * time.Second
		}
		_, _ = model.handleBackendEvent(Event{OperationID: "op-current", Kind: EventState, State: state, RetryAfter: retry})
		if model.state != state || stateLabel(state) == "" {
			t.Fatalf("state = %s", state)
		}
	}
	before := len(model.timeline)
	_, _ = model.handleBackendEvent(Event{OperationID: "op-stale", Kind: EventTextDelta, Delta: "stale"})
	if len(model.timeline) != before {
		t.Fatal("stale operation event changed timeline")
	}
	model.eventChannel = make(chan Event, 1)
	_, _ = model.handleBackendEvent(Event{OperationID: "op-current", Kind: EventTextDelta, Delta: "hello"})
	call := protocol.ToolCall{ID: "call-1", Name: "read_file", Arguments: []byte(`{"path":"main.go"}`)}
	_, _ = model.handleBackendEvent(Event{OperationID: "op-current", Kind: EventToolStart, ToolCall: &call})
	response := make(chan bool, 1)
	_, _ = model.handleBackendEvent(Event{OperationID: "op-current", Kind: EventApproval, Approval: &ApprovalRequest{Tool: "read_file", Risk: "read", Step: 1, Total: 1, Response: response}})
	if model.state != StateAwaitingApproval {
		t.Fatalf("state = %s", model.state)
	}
	_, _ = model.handleKey(tea.KeyPressMsg(tea.Key{Code: 'y', Text: "y"}))
	if !<-response || model.approval != nil {
		t.Fatal("approval response was not delivered")
	}
	result := protocol.ToolResult{CallID: "call-1", Content: "package main"}
	_, _ = model.handleBackendEvent(Event{OperationID: "op-current", Kind: EventToolResult, ToolResult: &result})
	if model.timeline[len(model.timeline)-1].tool.running {
		t.Fatal("tool remained running")
	}

	cancelled := false
	model.state = StateStreaming
	model.cancel = func() { cancelled = true }
	_, _ = model.handleKey(tea.KeyPressMsg(tea.Key{Code: 'c', Mod: tea.ModCtrl}))
	if !cancelled || model.state != StateCancelling {
		t.Fatalf("cancelled=%t state=%s", cancelled, model.state)
	}
}

func TestStaticNarrowViewportPasswordMaskAndContextPanel(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)}
	backend := &fakeBackend{snapshot: Snapshot{
		Mode: "auto", Provider: "work-provider-with-long-name", Model: "model-with-a-very-long-id",
		Context:   contextledger.Report{InputTokens: 50, OutputReserve: 10, TotalTokens: 60, ContextWindow: 100, LimitKnown: true, Percent: 60, Categories: []contextledger.CategoryUsage{{Label: "User messages", Tokens: 50, Percent: 100, Measurement: "estimated"}}},
		Providers: []ProviderItem{{Name: "work", Adapter: "openai_responses", Model: "model", Active: true}},
		Skills:    []SkillItem{{Name: "demo-skill", Source: "project", Status: "active", Activated: true}},
	}}
	model := NewModel(backend, Options{NoAnimation: true, NoColor: true, Clock: clock, Width: 40, Height: 16})
	model.snapshot = backend.snapshot
	model.timeline = []timelineItem{
		{kind: timelineMessage, role: "user", text: "检查中文和 narrow layout"},
		{kind: timelineMessage, role: "agent", text: "# Result\n完成"},
		{kind: timelineTool, tool: &toolView{name: "read_file", arguments: `{"path":"a-very-long-file-name.go"}`, content: "ok"}},
	}
	model.state = StateWaitingFirstToken
	model.startedAt = clock.now.Add(-time.Second)
	model.refreshViewport()
	view := model.View().Content
	if strings.Contains(view, "\x1b[") || !strings.Contains(view, "检查中文") || !strings.Contains(view, "read_file") || !strings.Contains(view, "...") {
		t.Fatalf("view = %q", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if lipgloss.Width(line) > 40 {
			t.Fatalf("line width %d: %q", lipgloss.Width(line), line)
		}
	}

	model.form = newProviderFormModel(ProviderForm{Name: "work", BaseURL: "https://example.com/v1", Model: "model", Adapter: "openai_responses"}, 40)
	model.form.inputs[providerFieldKey].SetValue("secret-value")
	if rendered := model.form.view(model.styles); strings.Contains(rendered, "secret-value") || !strings.Contains(rendered, "********") {
		t.Fatalf("password form = %q", rendered)
	}

	model.screen = screenContext
	model.contextExpand = true
	model.refreshViewport()
	if content := model.viewport.GetContent(); !strings.Contains(content, "Context") || !strings.Contains(content, "[==================]") {
		t.Fatalf("context panel = %q", content)
	}
}

func TestBackendCommandEventOrderingAndError(t *testing.T) {
	backend := &fakeBackend{submit: func(_ context.Context, operationID, _ string, emit func(Event)) error {
		emit(Event{OperationID: operationID, Kind: EventState, State: StateWaitingFirstToken})
		emit(Event{OperationID: operationID, Kind: EventTextDelta, Delta: "done"})
		return nil
	}}
	events := make(chan Event, 8)
	message := runBackendCmd(context.Background(), backend, "op-1", "hello", events)()
	if _, ok := message.(backendWorkerMsg); !ok {
		t.Fatalf("worker message = %#v", message)
	}
	var kinds []EventKind
	for event := range events {
		kinds = append(kinds, event.Kind)
	}
	if len(kinds) != 3 || kinds[0] != EventState || kinds[1] != EventTextDelta || kinds[2] != EventState {
		t.Fatalf("event order = %#v", kinds)
	}

	backend.submit = nil
	backend.err = errors.New("network failed")
	events = make(chan Event, 4)
	_ = runBackendCmd(context.Background(), backend, "op-2", "hello", events)()
	foundError := false
	for event := range events {
		foundError = foundError || event.Kind == EventNotice && event.Error
	}
	if !foundError {
		t.Fatal("backend error event was not marked")
	}
}

func TestProviderFormValidationAndModelFiltering(t *testing.T) {
	form := newProviderFormModel(ProviderForm{Adapter: "openai_responses"}, 80)
	if _, err := form.value(); err == nil {
		t.Fatal("empty provider form was accepted")
	}
	form.inputs[providerFieldName].SetValue("work")
	form.inputs[providerFieldURL].SetValue("https://example.com/v1")
	form.inputs[providerFieldModel].SetValue("manual-model")
	form.inputs[providerFieldContext].SetValue("32000")
	value, err := form.value()
	if err != nil || value.ContextWindow != 32000 || value.Model != "manual-model" {
		t.Fatalf("value=%#v err=%v", value, err)
	}
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true})
	model.models = []string{"alpha", "beta-code", "beta-chat"}
	model.modelFilter.SetValue("code")
	filtered := model.filteredModels()
	if len(filtered) != 1 || filtered[0] != "beta-code" {
		t.Fatalf("filtered = %#v", filtered)
	}
}

func TestResizeStormLongWordsAndStaleAnimationTick(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	model := NewModel(&fakeBackend{}, Options{Clock: clock, Width: 100, Height: 30})
	for index := 0; index < 100; index++ {
		width := 32 + index%90
		height := 10 + index%35
		_, _ = model.Update(tea.WindowSizeMsg{Width: width, Height: height})
	}
	if model.width < 40 || model.height < 12 || model.viewport.Width() != model.width || model.viewport.Height() != model.height-8 {
		t.Fatalf("size=%dx%d viewport=%dx%d", model.width, model.height, model.viewport.Width(), model.viewport.Height())
	}
	word := strings.Repeat("界", 50)
	for _, line := range strings.Split(wrapPlain(word, 12), "\n") {
		if lipgloss.Width(line) > 12 {
			t.Fatalf("long word line width=%d line=%q", lipgloss.Width(line), line)
		}
	}
	model.operationID = "current"
	model.state = StateStreaming
	before := model.spinner.View()
	_, _ = model.Update(operationSpinnerMsg{operationID: "stale", message: model.spinner.Tick()})
	if model.spinner.View() != before {
		t.Fatal("stale animation tick changed spinner")
	}
	model.snapshot.Provider = "provider-with-an-extremely-long-name"
	model.snapshot.Model = "model-with-an-extremely-long-name"
	view := model.View().Content
	if !strings.Contains(view, "\x1b[") {
		t.Fatal("color-capable view did not render ANSI styles")
	}
	for _, line := range strings.Split(ansi.Strip(view), "\n") {
		if lipgloss.Width(line) > model.width {
			t.Fatalf("line width=%d width=%d line=%q", lipgloss.Width(line), model.width, line)
		}
	}
}
