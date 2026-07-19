package ui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

func TestStreamingActivityShowsEstimatedExactTokensAndThinking(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 19, 12, 0, 14, 0, time.UTC)}
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Clock: clock, Width: 100, Height: 24})
	model.operationID = "op-activity"
	model.eventChannel = make(chan Event, 8)
	model.startedAt = clock.now.Add(-14 * time.Second)

	_, _ = model.handleBackendEvent(Event{OperationID: "op-activity", Kind: EventActivity, Activity: &Activity{Reasoning: true, ReasoningKnown: true, TokenBytesPerToken: 4, InputTokens: 1200}})
	_, _ = model.handleBackendEvent(Event{OperationID: "op-activity", Kind: EventReasoningDelta, Delta: strings.Repeat("r", 204)})
	_, _ = model.handleBackendEvent(Event{OperationID: "op-activity", Kind: EventTextDelta, Delta: strings.Repeat("x", 2936)})
	estimated := ansi.Strip(model.renderLoading())
	for _, expected := range []string{"Composing...", "14s", "↑ ≈1200 sent", "↓ ≈734 received", "thinking ≈51 tokens"} {
		if !strings.Contains(estimated, expected) {
			t.Fatalf("estimated activity missing %q: %q", expected, estimated)
		}
	}

	usage := protocol.Usage{InputTokens: 1100, OutputTokens: 700, ReasoningTokens: 320, Exact: true}
	_, _ = model.handleBackendEvent(Event{OperationID: "op-activity", Kind: EventUsage, Usage: &usage})
	exact := ansi.Strip(model.renderLoading())
	for _, expected := range []string{"↑ 1100 sent", "↓ 700 received", "thinking 320 tokens"} {
		if !strings.Contains(exact, expected) {
			t.Fatalf("exact activity missing %q: %q", expected, exact)
		}
	}
	if strings.Contains(exact, "≈") {
		t.Fatalf("exact activity remained estimated: %q", exact)
	}
	model.state = StateExecutingTool
	if toolActivity := ansi.Strip(model.renderLoading()); strings.Contains(toolActivity, "thinking") {
		t.Fatalf("tool activity reported active thinking: %q", toolActivity)
	}
	model.state = StateWaitingFirstToken

	_, _ = model.handleBackendEvent(Event{OperationID: "op-activity", Kind: EventActivity, Activity: &Activity{InputTokens: 1500, TokenBytesPerToken: 4}})
	pending := ansi.Strip(model.renderLoading())
	for _, expected := range []string{"Thinking...", "↑ ≈1500 sent"} {
		if !strings.Contains(pending, expected) {
			t.Fatalf("pending activity missing %q: %q", expected, pending)
		}
	}
	if strings.Contains(pending, "thinking 320") || strings.Contains(pending, "+ tokens") {
		t.Fatalf("pending activity reused a prior-round token count: %q", pending)
	}
	_, _ = model.handleBackendEvent(Event{OperationID: "op-activity", Kind: EventReasoningDelta, Delta: strings.Repeat("z", 40)})
	_, _ = model.handleBackendEvent(Event{OperationID: "op-activity", Kind: EventTextDelta, Delta: strings.Repeat("y", 40)})
	nextTurn := ansi.Strip(model.renderLoading())
	for _, expected := range []string{"↓ ≈710 received", "thinking ≈10 tokens"} {
		if !strings.Contains(nextTurn, expected) {
			t.Fatalf("next turn activity missing %q: %q", expected, nextTurn)
		}
	}
	inexactUsage := protocol.Usage{}
	_, _ = model.handleBackendEvent(Event{OperationID: "op-activity", Kind: EventUsage, Usage: &inexactUsage})
	if local := ansi.Strip(model.renderLoading()); !strings.Contains(local, "thinking ≈10 tokens") {
		t.Fatalf("local thinking estimate was lost without exact usage: %q", local)
	}

	model.resize(40, 16)
	if width := lipgloss.Width(ansi.Strip(model.renderLoading())); width > 40 {
		t.Fatalf("activity width=%d line=%q", width, ansi.Strip(model.renderLoading()))
	}
}

func TestInterruptRequestCancelsThenQuits(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true})
	model.operationID = "op-interrupt"
	model.state = StatePreparingTool
	cancelled := false
	model.cancel = func() { cancelled = true }

	_, command := model.Update(interruptRequestMsg{})
	if !cancelled || !model.cancelRequested || model.state != StateCancelling || command != nil {
		t.Fatalf("cancelled=%t requested=%t state=%s command=%v", cancelled, model.cancelRequested, model.state, command)
	}
	_, command = model.Update(interruptRequestMsg{})
	if command == nil {
		t.Fatal("second interrupt did not request exit")
	}
	if _, ok := command().(tea.QuitMsg); !ok {
		t.Fatalf("second interrupt message = %#v", command())
	}
}

func TestChatInputBandUsesOnePromptAndNativeCursor(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	_ = model.input.Focus()
	model.input.SetValue("html写一个你的自我介绍页面")

	view := model.View()
	if model.input.Height() != 1 || model.input.VirtualCursor() {
		t.Fatalf("height=%d virtual_cursor=%t", model.input.Height(), model.input.VirtualCursor())
	}
	if strings.Count(view.Content, "> ") != 1 || !strings.Contains(view.Content, "html写一个你的自我介绍页面") {
		t.Fatalf("input view = %q", view.Content)
	}
	if strings.Count(view.Content, strings.Repeat("─", model.width)) != 2 {
		t.Fatalf("input rules missing: %q", view.Content)
	}
	if view.Cursor == nil || view.Cursor.Position.X < 2 || view.Cursor.Position.Y <= model.viewport.Height() {
		t.Fatalf("cursor = %#v viewport_height=%d", view.Cursor, model.viewport.Height())
	}
	model.approval = &ApprovalRequest{Tool: "write_file", Risk: "write"}
	if cursor := model.View().Cursor; cursor != nil {
		t.Fatalf("approval cursor = %#v", cursor)
	}
}

func TestTimelineMarkdownCacheInvalidatesOnTextAndWidth(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, Width: 80, Height: 24})
	model.timeline = []timelineItem{{kind: timelineMessage, role: "agent", text: "**cached response**"}}
	model.refreshViewport()
	if model.timeline[0].renderedText == "" {
		t.Fatal("markdown was not cached")
	}

	model.timeline[0].renderedText = "CACHE_SENTINEL"
	model.refreshViewport()
	if !strings.Contains(model.viewport.GetContent(), "CACHE_SENTINEL") {
		t.Fatal("unchanged markdown missed cache")
	}

	model.timeline[0].text += " updated"
	model.refreshViewport()
	if strings.Contains(model.viewport.GetContent(), "CACHE_SENTINEL") {
		t.Fatal("text change did not invalidate markdown cache")
	}
	model.timeline[0].renderedText = "WIDTH_SENTINEL"
	model.resize(72, 24)
	if strings.Contains(model.viewport.GetContent(), "WIDTH_SENTINEL") {
		t.Fatal("width change did not invalidate markdown cache")
	}
}

func TestLocalMarkdownLinkTargetsContainingWorkspaceDirectory(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "build")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(workspace, "index.html")
	if err := os.WriteFile(file, []byte("<main>Eylu</main>"), 0o600); err != nil {
		t.Fatal(err)
	}
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, Width: 100, Height: 24})
	model.snapshot.Workspace = workspace

	rendered := model.renderMarkdown("已完成：[index.html](/build/index.html) [docs](https://example.com/docs)")
	directoryURL := directoryFileURL(workspace)
	if !strings.Contains(rendered, ";"+directoryURL+"\x07") {
		t.Fatalf("local directory hyperlink missing: %q want=%q", rendered, directoryURL)
	}
	if !strings.Contains(rendered, ";https://example.com/docs\x07") {
		t.Fatalf("external hyperlink changed: %q", rendered)
	}
	toolRendered := model.renderTool(&toolView{name: "write_file", path: "index.html", generatedBytes: 17, generatedLines: 1})
	if !strings.Contains(toolRendered, ";"+directoryURL+"\x07") {
		t.Fatalf("tool file hyperlink missing: %q", toolRendered)
	}
	longDirectory := strings.Repeat("nested-", 10)
	longRelativePath := filepath.Join(longDirectory, "a-very-long-generated-file-name.html")
	if err := os.MkdirAll(filepath.Join(workspace, longDirectory), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, longRelativePath), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	model.resize(40, 24)
	longToolRendered := model.renderTool(&toolView{name: "write_file", path: longRelativePath, generatedBytes: 2, generatedLines: 1})
	if strings.Count(longToolRendered, "\x1b]8;") != 2 {
		t.Fatalf("truncated hyperlink sequence is unbalanced: %q", longToolRendered)
	}
	for _, line := range strings.Split(ansi.Strip(longToolRendered), "\n") {
		if lipgloss.Width(line) > model.width {
			t.Fatalf("long tool line width=%d line=%q", lipgloss.Width(line), line)
		}
	}
}

func TestCancelledBackendDoesNotBlockOnFullPreviewQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	backend := &fakeBackend{submit: func(ctx context.Context, operationID, _ string, emit func(Event)) error {
		for index := 0; index < 1024; index++ {
			emit(Event{OperationID: operationID, Kind: EventToolCallDelta, ToolCallDelta: &protocol.ToolCallDelta{ID: "call", Delta: "x"}})
		}
		return ctx.Err()
	}}
	events := make(chan Event, 1)
	done := make(chan tea.Msg, 1)
	go func() { done <- runBackendCmd(ctx, backend, "op-cancel", "prompt", events)() }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled backend remained blocked on preview events")
	}
}

func TestToolCallDeltaRendersLiveFileChangeWithoutDuplicateTool(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	model.operationID = "op-live"
	model.eventChannel = make(chan Event, 8)
	updates := []protocol.ToolCallDelta{
		{ID: "call-write", Name: "write_file", Delta: `{"path":"index.html","content":"<main>\n`},
		{ID: "call-write", Name: "write_file", Delta: `Hello</main>"}`},
		{ID: "call-write", Name: "write_file", Arguments: `{"path":"index.html","content":"<main>\nHello</main>"}`, Done: true},
	}
	for index := range updates {
		_, _ = model.handleBackendEvent(Event{OperationID: "op-live", Kind: EventToolCallDelta, ToolCallDelta: &updates[index]})
	}
	if len(model.timeline) != 1 || model.state != StatePreparingTool {
		t.Fatalf("timeline=%#v state=%s", model.timeline, model.state)
	}
	tool := model.timeline[0].tool
	if tool.path != "index.html" || tool.generatedBytes == 0 || !strings.Contains(tool.preview, "+ Hello</main>") {
		t.Fatalf("tool = %#v", tool)
	}
	view := model.viewport.GetContent()
	if !strings.Contains(view, "write_file") || !strings.Contains(view, "generating") || !strings.Contains(view, "index.html") {
		t.Fatalf("view = %q", view)
	}

	call := protocol.ToolCall{ID: "call-write", Name: "write_file", Arguments: []byte(updates[2].Arguments)}
	_, _ = model.handleBackendEvent(Event{OperationID: "op-live", Kind: EventToolStart, ToolCall: &call})
	if len(model.timeline) != 1 || model.timeline[0].tool.preparing || !model.timeline[0].tool.running {
		t.Fatalf("tool start duplicated or lost state: %#v", model.timeline)
	}
	result := protocol.ToolResult{CallID: "call-write", Content: "wrote 25 bytes to index.html", Metadata: map[string]any{"path": "index.html", "bytes": 25}}
	_, _ = model.handleBackendEvent(Event{OperationID: "op-live", Kind: EventToolResult, ToolResult: &result})
	if model.timeline[0].tool.running || model.timeline[0].tool.isError {
		t.Fatalf("tool result = %#v", model.timeline[0].tool)
	}
}

func TestPartialJSONStringFieldAndLargePreviewStayBounded(t *testing.T) {
	arguments := `{"path":"index.html","content":"<h1>你好<\/h1>\n\"quoted\""}`
	path, pathStarted := partialJSONStringField(arguments, "path")
	content, contentStarted := partialJSONStringField(arguments, "content")
	if !pathStarted || path != "index.html" || !contentStarted || content != "<h1>你好</h1>\n\"quoted\"" {
		t.Fatalf("path=%q pathStarted=%t content=%q contentStarted=%t", path, pathStarted, content, contentStarted)
	}
	partial, started := partialJSONStringField(`{"content":"line 1\nline 2\u4f`, "content")
	if !started || partial != "line 1\nline 2" {
		t.Fatalf("partial=%q started=%t", partial, started)
	}

	view := &toolView{name: "write_file", arguments: `{"path":"large.txt","content":"` + strings.Repeat(`line\n`, 100)}
	updateFileToolPreview(view)
	if view.generatedLines != 101 || len(strings.Split(view.preview, "\n")) > 6 || len(view.preview) > 128 {
		t.Fatalf("lines=%d preview=%q", view.generatedLines, view.preview)
	}

	streamed := &toolView{name: "write_file"}
	streamed.appendArguments(`{"path":"streamed.txt","content":"`)
	for index := 0; index < 1000; index++ {
		streamed.appendArguments(strings.Repeat("x", 64))
		if len(streamed.arguments)-streamed.previewArgumentBytes >= 256 {
			updateFileToolPreview(streamed)
		}
	}
	if len(streamed.arguments) != len(`{"path":"streamed.txt","content":"`)+64_000 || len(streamed.preview) > 2048 {
		t.Fatalf("arguments=%d preview=%d", len(streamed.arguments), len(streamed.preview))
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
	if model.width < 40 || model.height < 12 || model.viewport.Width() != model.width || model.viewport.Height() != model.height-6 {
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
