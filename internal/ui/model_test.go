package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	snapshot      Snapshot
	submit        func(context.Context, string, Submission, func(Event)) error
	compact       func(context.Context, string, func(Event)) error
	err           error
	command       string
	models        []string
	files         []FileItem
	mcpServers    []MCPServerItem
	mcpActions    []string
	mcpCalls      int
	mode          string
	selection     ModelSelection
	contextWindow int
}

func (b *fakeBackend) Snapshot(context.Context) (Snapshot, error) { return b.snapshot, b.err }
func (b *fakeBackend) Submit(ctx context.Context, operationID string, submission Submission, emit func(Event)) error {
	if b.submit != nil {
		return b.submit(ctx, operationID, submission, emit)
	}
	return b.err
}
func (b *fakeBackend) Compact(ctx context.Context, operationID string, emit func(Event)) error {
	if b.compact != nil {
		return b.compact(ctx, operationID, emit)
	}
	return b.err
}
func (b *fakeBackend) Command(_ context.Context, command string) (string, error) {
	b.command = command
	return "command ok", b.err
}
func (b *fakeBackend) ListFiles(context.Context) ([]FileItem, error) {
	return append([]FileItem(nil), b.files...), b.err
}
func (b *fakeBackend) SetMode(_ context.Context, mode string) error {
	b.mode = mode
	return b.err
}
func (b *fakeBackend) UpsertProvider(context.Context, ProviderForm) (ModelSelection, error) {
	return b.selection, b.err
}
func (b *fakeBackend) DeleteProvider(context.Context, string) error { return b.err }
func (b *fakeBackend) UseProvider(context.Context, string) error    { return b.err }
func (b *fakeBackend) SetModel(context.Context, string, string) (ModelSelection, error) {
	return b.selection, b.err
}
func (b *fakeBackend) SetContextWindow(_ context.Context, _ string, value int) error {
	b.contextWindow = value
	return b.err
}
func (b *fakeBackend) FetchModels(context.Context, string) ([]string, error) {
	return append([]string(nil), b.models...), b.err
}
func (b *fakeBackend) MCPServers(context.Context) ([]MCPServerItem, error) {
	b.mcpCalls++
	return append([]MCPServerItem(nil), b.mcpServers...), b.err
}
func (b *fakeBackend) MCPAction(_ context.Context, server string, action MCPAction) error {
	b.mcpActions = append(b.mcpActions, server+":"+string(action))
	return b.err
}

func TestStartupLoadsMCPWithAnimatedStatusBelowBanner(t *testing.T) {
	backend := &fakeBackend{mcpServers: []MCPServerItem{{Name: "search", Status: "connected", ToolCount: 5}}}
	model := NewModel(backend, Options{NoColor: true, Version: "1.2.3", Workspace: "E:/Eylu", Width: 80, Height: 24})
	command := model.Init()
	if command == nil || !model.mcpLoading {
		t.Fatalf("startup MCP state: command=%v loading=%t", command, model.mcpLoading)
	}
	loading := ansi.Strip(model.renderTimeline())
	if banner, status := strings.Index(loading, "v1.2.3"), strings.Index(loading, "MCP  Loading servers..."); banner < 0 || status <= banner {
		t.Fatalf("MCP loading status is not below banner:\n%s", loading)
	}

	batch, ok := command().(tea.BatchMsg)
	if !ok {
		t.Fatalf("Init message=%T, want tea.BatchMsg", command())
	}
	var serverResult mcpServersMsg
	serverResultFound := false
	spinnerBefore := model.mcpSpinner.View()
	spinnerAdvanced := false
	for _, item := range batch {
		message := item()
		switch typed := message.(type) {
		case mcpServersMsg:
			serverResult = typed
			serverResultFound = true
		case mcpSpinnerMsg:
			_, _ = model.Update(typed)
			spinnerAdvanced = model.mcpSpinner.View() != spinnerBefore
		}
	}
	if backend.mcpCalls != 1 || !serverResultFound || !spinnerAdvanced {
		t.Fatalf("startup MCP commands: calls=%d result=%t spinner_advanced=%t", backend.mcpCalls, serverResultFound, spinnerAdvanced)
	}
	_, _ = model.Update(serverResult)
	_, _ = model.Update(mcpSpinnerMsg{message: model.mcpSpinner.Tick()})
	ready := ansi.Strip(model.renderTimeline())
	if model.mcpLoading || strings.Contains(ready, "MCP  ") || strings.Contains(ready, "Loading servers") {
		t.Fatalf("completed MCP loading row was not cleared:\n%s", ready)
	}
}

func TestStartupMCPFailureClearsBannerAndStaysInConversation(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	_ = model.Init()
	_, _ = model.Update(mcpServersMsg{servers: []MCPServerItem{{Name: "search", Status: "error", LastError: "tools/list failed"}}})
	rendered := ansi.Strip(model.renderTimeline())
	if !strings.Contains(rendered, "MCP search: tools/list failed") {
		t.Fatalf("startup MCP failure missing from conversation:\n%s", rendered)
	}
	if strings.Contains(rendered, "Loading servers") || strings.Contains(rendered, "1 failed · /mcp") {
		t.Fatalf("completed MCP loading row was not cleared:\n%s", rendered)
	}
}

func TestMCPBadGatewayNoticeIsConciseAndActionable(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 100, Height: 24})
	_ = model.Init()
	_, _ = model.Update(mcpServersMsg{servers: []MCPServerItem{{
		Name: "tavily-hikari", Status: "error",
		LastError: `connect MCP server: calling "initialize": rejected by transport: sending "initialize": Bad Gateway`,
	}}})
	rendered := ansi.Strip(model.renderTimeline())
	if !strings.Contains(rendered, "MCP tavily-hikari: HTTP 502 Bad Gateway · /mcp then r to reconnect") {
		t.Fatalf("actionable gateway notice missing:\n%s", rendered)
	}
	if strings.Contains(rendered, `calling "initialize"`) {
		t.Fatalf("raw gateway error leaked into conversation:\n%s", rendered)
	}
}

func TestOpeningMCPDuringStartupDoesNotStartDuplicateLoad(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	_ = model.Init()
	_, command := model.executeSlash("/mcp")
	if model.screen != screenMCP || command != nil {
		t.Fatalf("screen=%s duplicate command=%v", model.screen, command)
	}
}

func TestMCPStartupStatusFitsNarrowViewport(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 40, Height: 20})
	_ = model.Init()
	status := ansi.Strip(model.renderMCPStartupStatus())
	if width := lipgloss.Width(status); width > model.viewportContentWidth() {
		t.Fatalf("MCP status width=%d available=%d status=%q", width, model.viewportContentWidth(), status)
	}
}

func TestMCPSpinnerSkipsChatRefreshWhenBannerIsHidden(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoColor: true, Width: 80, Height: 20})
	_ = model.Init()
	model.followOutput = false
	model.timeline = []timelineItem{{kind: timelineMessage, role: "user", text: strings.Repeat("history line\n", 40)}}
	model.refreshViewport()
	model.viewport.SetYOffset(bannerViewportRows)
	before := model.viewport.GetContent()
	_, _ = model.Update(mcpSpinnerMsg{message: model.mcpSpinner.Tick()})
	if model.viewport.GetContent() != before {
		t.Fatal("hidden MCP startup status caused a full chat viewport refresh")
	}
}

func TestMCPPanelListsDetailsCatalogsAndActions(t *testing.T) {
	statuses := []string{"disabled", "connecting", "connected", "needs_auth", "reconnecting", "disconnected", "error"}
	servers := make([]MCPServerItem, 0, len(statuses))
	for index, status := range statuses {
		servers = append(servers, MCPServerItem{
			Name: fmt.Sprintf("server-%d", index), Status: status, Transport: "streamable_http", ProtocolVersion: "2025-11-25",
			ToolCount: 1, ResourceCount: 1, PromptCount: 1,
		})
	}
	servers[0].LastError = "authentication expired"
	servers[0].Implementation = "fixture-mcp"
	servers[0].Version = "1.2.3"
	servers[0].ConnectDurationMS = 42
	servers[0].Config = `{"url":"https://example.test/mcp","authorization":"[REDACTED]"}`
	servers[0].Capabilities = `{"tools":{"listChanged":true}}`
	servers[0].Instructions = "Use this server for fixture data."
	servers[0].Diagnostics = "last ping 12ms"
	servers[0].Tools = []MCPToolItem{{Name: "lookup", LocalName: "mcp__server_0__lookup", Description: "Look up a fixture", Annotations: `{"readOnlyHint":true}`, InputSchema: `{"type":"object"}`, OutputSchema: `{"type":"object"}`, Permission: "read", Status: "available"}}
	servers[0].Resources = []MCPResourceItem{{URI: "fixture://catalog", Name: "catalog", Description: "Fixture catalog", MIMEType: "application/json"}}
	servers[0].Prompts = []MCPPromptItem{{Name: "summarize", Description: "Summarize fixture data", Arguments: `[{"name":"subject"}]`}}
	backend := &fakeBackend{mcpServers: servers}
	model := NewModel(backend, Options{NoAnimation: true, NoColor: true, Width: 120, Height: 34})

	_, command := model.executeSlash("/mcp")
	if model.screen != screenMCP || command == nil {
		t.Fatalf("screen=%s command=%v", model.screen, command)
	}
	_, _ = model.Update(command())
	listView := ansi.Strip(model.View().Content)
	for _, expected := range append(statuses, "authentication expired") {
		if !strings.Contains(listView, expected) {
			t.Fatalf("MCP list missing %q:\n%s", expected, listView)
		}
	}

	_, _ = model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if model.screen != screenMCPDetail {
		t.Fatalf("detail screen=%s", model.screen)
	}
	detailView := ansi.Strip(model.View().Content)
	for _, expected := range []string{"fixture-mcp", "1.2.3", "42ms", "listChanged", "fixture data"} {
		if !strings.Contains(detailView, expected) {
			t.Fatalf("MCP detail missing %q:\n%s", expected, detailView)
		}
	}

	_, _ = model.handleKey(tea.KeyPressMsg(tea.Key{Code: '2', Text: "2"}))
	toolsView := ansi.Strip(model.View().Content)
	for _, expected := range []string{"Tools", "lookup", "available", "read"} {
		if !strings.Contains(toolsView, expected) {
			t.Fatalf("MCP tools list missing %q:\n%s", expected, toolsView)
		}
	}
	for _, hidden := range []string{"mcp__server_0__lookup", "readOnlyHint", "Input schema"} {
		if strings.Contains(toolsView, hidden) {
			t.Fatalf("MCP tools list leaked detail %q:\n%s", hidden, toolsView)
		}
	}
	_, _ = model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if model.screen != screenMCPToolDetail {
		t.Fatalf("tool detail screen=%s", model.screen)
	}
	toolDetail := ansi.Strip(model.View().Content)
	for _, expected := range []string{"lookup", "mcp__server_0__lookup", "readOnlyHint", "Input schema", "Look up a fixture"} {
		if !strings.Contains(toolDetail, expected) {
			t.Fatalf("MCP tool detail missing %q:\n%s", expected, toolDetail)
		}
	}
	_, _ = model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	if model.screen != screenMCPDetail || model.mcpTab != 1 {
		t.Fatalf("tool detail back screen=%s tab=%d", model.screen, model.mcpTab)
	}

	for _, catalog := range []struct {
		key      string
		expected []string
	}{
		{key: "3", expected: []string{"Resources", "fixture://catalog", "application/json"}},
		{key: "4", expected: []string{"Prompts", "summarize", "subject"}},
	} {
		_, _ = model.handleKey(tea.KeyPressMsg(tea.Key{Code: rune(catalog.key[0]), Text: catalog.key}))
		view := ansi.Strip(model.View().Content)
		for _, expected := range catalog.expected {
			if !strings.Contains(view, expected) {
				t.Fatalf("MCP catalog %s missing %q:\n%s", catalog.key, expected, view)
			}
		}
	}

	for _, action := range []struct {
		key  rune
		name MCPAction
	}{{'r', MCPActionReconnect}, {'e', MCPActionEnable}, {'d', MCPActionDisable}, {'l', MCPActionLogin}, {'o', MCPActionLogout}} {
		_, actionCommand := model.handleKey(tea.KeyPressMsg(tea.Key{Code: action.key, Text: string(action.key)}))
		if actionCommand == nil {
			t.Fatalf("action %s returned no command", action.name)
		}
		_, _ = model.Update(actionCommand())
	}
	if got := strings.Join(backend.mcpActions, ","); got != "server-0:reconnect,server-0:enable,server-0:disable,server-0:login,server-0:logout" {
		t.Fatalf("actions=%q", got)
	}
}

func TestMCPCompletionExecutesExactCommandWithoutTrailingSpace(t *testing.T) {
	backend := &fakeBackend{}
	model := NewModel(backend, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	model.input.SetValue("/mcp")
	_ = model.refreshCompletion()

	_, command := model.handleChatKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if model.screen != screenMCP || command == nil {
		t.Fatalf("screen=%s command=%v input=%q completion=%#v", model.screen, command, model.input.Value(), model.completion)
	}
}

func TestMCPDetailUsesArrowKeysForTabs(t *testing.T) {
	backend := &fakeBackend{mcpServers: []MCPServerItem{{Name: "remote", Status: "connected"}}}
	model := NewModel(backend, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	_, fetch := model.executeSlash("/mcp")
	_, _ = model.Update(fetch())
	_, _ = model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))

	_, _ = model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyRight}))
	if model.mcpTab != 1 {
		t.Fatalf("right selected tab %d, want 1", model.mcpTab)
	}
	_, _ = model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyLeft}))
	if model.mcpTab != 0 {
		t.Fatalf("left selected tab %d, want 0", model.mcpTab)
	}
}

func TestMCPConnectionErrorAppearsOnceInTimelineAndDetailStaysConcise(t *testing.T) {
	const connectionError = "connect MCP server: context deadline exceeded"
	backend := &fakeBackend{mcpServers: []MCPServerItem{{
		Name: "remote", Status: "reconnecting", Transport: "streamable_http", LastError: connectionError,
		Config: "CONFIG_SENTINEL", Diagnostics: "DIAGNOSTIC_SENTINEL",
	}}}
	model := NewModel(backend, Options{NoAnimation: true, NoColor: true, Width: 100, Height: 28})
	_, fetch := model.executeSlash("/mcp")
	_, _ = model.Update(fetch())
	_, _ = model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))

	detail := ansi.Strip(model.View().Content)
	if !strings.Contains(detail, connectionError) || strings.Contains(detail, "CONFIG_SENTINEL") || strings.Contains(detail, "DIAGNOSTIC_SENTINEL") {
		t.Fatalf("unexpected MCP detail:\n%s", detail)
	}
	if len(model.timeline) != 1 || !model.timeline[0].err || !strings.Contains(model.timeline[0].text, connectionError) {
		t.Fatalf("timeline=%#v", model.timeline)
	}

	_, _ = model.Update(mcpServersMsg{servers: backend.mcpServers})
	if len(model.timeline) != 1 {
		t.Fatalf("repeated poll duplicated MCP error: %#v", model.timeline)
	}
	_, _ = model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	_, _ = model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	if timeline := ansi.Strip(model.renderTimeline()); !strings.Contains(timeline, connectionError) {
		t.Fatalf("MCP error missing from chat timeline:\n%s", timeline)
	}
}

func TestMCPPollKeepsSingleTimer(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)}
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Clock: clock})
	_, fetch := model.executeSlash("/mcp")
	_, poll := model.Update(fetch())
	if poll == nil || !model.mcpPollScheduled {
		t.Fatalf("poll=%v scheduled=%t", poll, model.mcpPollScheduled)
	}
	if duplicate := model.mcpPollCmd(); duplicate != nil {
		t.Fatal("a second MCP poll was scheduled while one was pending")
	}
	_, nextFetch := model.Update(poll())
	if nextFetch == nil || model.mcpPollScheduled {
		t.Fatalf("next fetch=%v scheduled=%t", nextFetch, model.mcpPollScheduled)
	}
	_, nextPoll := model.Update(nextFetch())
	if nextPoll == nil || !model.mcpPollScheduled {
		t.Fatalf("next poll=%v scheduled=%t", nextPoll, model.mcpPollScheduled)
	}
}

func TestMCPPanelFitsNarrowViewport(t *testing.T) {
	backend := &fakeBackend{mcpServers: []MCPServerItem{{
		Name: "very-long-server-name-for-narrow-terminals", Status: "reconnecting", Transport: "streamable_http", ProtocolVersion: "2025-11-25",
		LastError: strings.Repeat("connection diagnostic ", 6), Capabilities: `{"tools":{"listChanged":true},"resources":{"subscribe":true}}`,
		Tools: []MCPToolItem{{Name: "a_very_long_tool_name", Description: strings.Repeat("description ", 8)}},
	}}}
	model := NewModel(backend, Options{NoAnimation: true, NoColor: true, Width: 40, Height: 18})
	_, fetch := model.executeSlash("/mcp")
	_, _ = model.Update(fetch())
	assertMCPViewFits := func(label string) {
		t.Helper()
		view := ansi.Strip(model.View().Content)
		if lipgloss.Height(view) != 18 {
			t.Fatalf("%s height=%d\n%s", label, lipgloss.Height(view), view)
		}
		for _, line := range strings.Split(view, "\n") {
			if lipgloss.Width(line) > 40 {
				t.Fatalf("%s width=%d line=%q", label, lipgloss.Width(line), line)
			}
		}
	}
	assertMCPViewFits("list")
	_, _ = model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	assertMCPViewFits("detail")
	_, _ = model.handleKey(tea.KeyPressMsg(tea.Key{Code: '2', Text: "2"}))
	assertMCPViewFits("tools")
}

func TestOperationStatesStaleMessagesApprovalAndCancellation(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)}
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Clock: clock, Width: 80, Height: 24})
	model.operationID = "op-current"
	model.startedAt = clock.now.Add(-time.Second)
	for _, state := range []OperationState{StateConnecting, StateFetchingModels, StateWaitingFirstToken, StateStreaming, StateExecutingTool, StateRetryBackoff, StateCancelling, StateCancelled, StateInterrupted, StateCompleted, StateFailed, StateIdle} {
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
	response := make(chan ApprovalDecision, 1)
	_, _ = model.handleBackendEvent(Event{OperationID: "op-current", Kind: EventApproval, Approval: &ApprovalRequest{Tool: "bash", Risk: "exec", Summary: "$ go test ./...", Reason: "Verify the implementation before reporting", PolicyReason: "manual mode requires confirmation", Step: 1, Total: 1, Response: response}})
	if model.state != StateAwaitingApproval {
		t.Fatalf("state = %s", model.state)
	}
	approvalView := ansi.Strip(model.View().Content)
	for _, expected := range []string{"Action approval", "Verify the implementation before reporting", "$ go test ./...", "Yes", "No"} {
		if !strings.Contains(approvalView, expected) {
			t.Fatalf("approval view missing %q:\n%s", expected, approvalView)
		}
	}
	_, _ = model.handleKey(tea.KeyPressMsg(tea.Key{Code: 'y', Text: "y"}))
	if decision := <-response; !decision.Approved || model.approval != nil {
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

func TestParallelToolCompletionKeepsExecutingStateUntilBatchFinishes(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	model.operationID = "op-parallel"
	model.eventChannel = make(chan Event, 1)
	first := protocol.ToolCall{ID: "first", Name: "read_file", Arguments: []byte(`{"path":"first.go"}`)}
	second := protocol.ToolCall{ID: "second", Name: "read_file", Arguments: []byte(`{"path":"second.go"}`)}
	_, _ = model.handleBackendEvent(Event{OperationID: model.operationID, Kind: EventToolStart, ToolCall: &first})
	_, _ = model.handleBackendEvent(Event{OperationID: model.operationID, Kind: EventToolStart, ToolCall: &second})
	_, _ = model.handleBackendEvent(Event{OperationID: model.operationID, Kind: EventToolResult, ToolResult: &protocol.ToolResult{CallID: "second", Content: "done"}})
	if model.state != StateExecutingTool {
		t.Fatalf("state=%s", model.state)
	}
	if len(model.timeline) != 2 || model.timeline[0].tool.callID != "first" || model.timeline[1].tool.callID != "second" || !model.timeline[0].tool.running || model.timeline[1].tool.running {
		t.Fatalf("timeline = %#v", model.timeline)
	}
	_, _ = model.handleBackendEvent(Event{OperationID: model.operationID, Kind: EventToolResult, ToolResult: &protocol.ToolResult{CallID: "first", Content: "done"}})
	if model.state != StateWaitingFirstToken {
		t.Fatalf("state=%s", model.state)
	}
}

func TestWebActivityAndCitationEventsRenderLifecycle(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	model.operationID = "op-web"
	model.eventChannel = make(chan Event, 2)
	running := protocol.WebActivity{CallID: "web-1", Kind: protocol.ToolWebSearch, Query: "Eylu", Status: protocol.WebStatusRunning}
	completed := running
	completed.Status = protocol.WebStatusCompleted
	completed.Sources = []protocol.WebSource{{URL: "https://example.com", Title: "Example"}}
	opened := protocol.WebActivity{CallID: "web-2", Kind: protocol.ToolWebSearch, URL: "https://example.com/page", Action: "open_page", Status: protocol.WebStatusCompleted}
	_, _ = model.handleBackendEvent(Event{OperationID: "op-web", Kind: EventWebActivity, WebActivity: &running})
	_, _ = model.handleBackendEvent(Event{OperationID: "op-web", Kind: EventWebActivity, WebActivity: &completed})
	_, _ = model.handleBackendEvent(Event{OperationID: "op-web", Kind: EventWebActivity, WebActivity: &opened})
	_, _ = model.handleBackendEvent(Event{OperationID: "op-web", Kind: EventCitation, Citation: &protocol.URLCitation{CallID: "web-1", URL: "https://example.com", Title: "Example"}})
	rendered := ansi.Strip(model.renderTimeline())
	for _, expected := range []string{"Web search · completed · 1 source", "Search  Eylu", "Open  https://example.com/page"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("missing %q:\n%s", expected, rendered)
		}
	}
	if strings.Contains(rendered, "0 sources") || strings.Count(rendered, "Web search") != 1 {
		t.Fatalf("web activity was not updated in place:\n%s", rendered)
	}
}

func TestLocalWebFunctionToolIsRenderedOnlyAsWebActivity(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	model.operationID = "op-web-function"
	call := protocol.ToolCall{ID: "batch", Name: "web_search", Arguments: json.RawMessage(`{"queries":["one","two"]}`)}
	activity := protocol.WebActivity{CallID: "batch:1", Kind: protocol.ToolWebSearch, Query: "one", Action: "search", Status: protocol.WebStatusRunning}
	_, _ = model.handleBackendEvent(Event{OperationID: model.operationID, Kind: EventToolStart, ToolCall: &call})
	_, _ = model.handleBackendEvent(Event{OperationID: model.operationID, Kind: EventWebActivity, WebActivity: &activity})
	rendered := ansi.Strip(model.renderTimeline())
	if strings.Contains(rendered, "> web_search") || !strings.Contains(rendered, "Web search · running") || !strings.Contains(rendered, "Search") || !strings.Contains(rendered, "one") {
		t.Fatalf("rendered local web tool =\n%s", rendered)
	}
}

func TestWebActivityGroupKeepsFiveNewestEntriesAndAnimatesOverflow(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	model := NewModel(&fakeBackend{}, Options{Clock: clock, NoColor: true, Width: 80, Height: 24})
	model.operationID = "op-web"
	for index := 1; index <= 5; index++ {
		activity := protocol.WebActivity{CallID: fmt.Sprintf("web-%d", index), Kind: protocol.ToolWebSearch, Query: fmt.Sprintf("query-%d", index), Action: "search", Status: protocol.WebStatusCompleted}
		_, _ = model.handleBackendEvent(Event{OperationID: "op-web", Kind: EventWebActivity, WebActivity: &activity})
	}
	sixth := protocol.WebActivity{CallID: "web-6", Kind: protocol.ToolWebSearch, Query: "query-6", Action: "search", Status: protocol.WebStatusRunning}
	_, command := model.handleBackendEvent(Event{OperationID: "op-web", Kind: EventWebActivity, WebActivity: &sixth})
	if command == nil {
		t.Fatal("overflow did not schedule the upward transition")
	}
	before := ansi.Strip(model.renderTimeline())
	if !strings.Contains(before, "query-1") || strings.Contains(before, "query-6") {
		t.Fatalf("initial animation frame =\n%s", before)
	}
	_, _ = model.Update(command())
	after := ansi.Strip(model.renderTimeline())
	if strings.Contains(after, "query-1") || !strings.Contains(after, "query-6") || !strings.Contains(after, "▸ … +1 hidden") {
		t.Fatalf("completed animation frame =\n%s", after)
	}
	seventh := protocol.WebActivity{CallID: "web-7", Kind: protocol.ToolWebSearch, Query: "query-7", Action: "search", Status: protocol.WebStatusRunning}
	_, command = model.handleBackendEvent(Event{OperationID: "op-web", Kind: EventWebActivity, WebActivity: &seventh})
	_, _ = model.Update(command())
	after = ansi.Strip(model.renderTimeline())
	if strings.Contains(after, "query-2") || !strings.Contains(after, "query-7") || !strings.Contains(after, "▸ … +2 hidden") {
		t.Fatalf("second animation frame =\n%s", after)
	}
}

func TestWebActivityBatchQueriesCountAsItemsAndToggleExpansion(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	model.operationID = "op-web"
	activity := protocol.WebActivity{
		CallID: "batch-1", Kind: protocol.ToolWebSearch, Action: "search", Status: protocol.WebStatusRunning,
		Queries: []string{"query-1", "query-2", "query-3", "query-4", "query-5", "query-6"},
	}
	_, _ = model.handleBackendEvent(Event{OperationID: "op-web", Kind: EventWebActivity, WebActivity: &activity})
	collapsed := ansi.Strip(model.renderTimeline())
	if strings.Contains(collapsed, "query-1") || !strings.Contains(collapsed, "query-6") || !strings.Contains(collapsed, "▸ … +1 hidden") {
		t.Fatalf("collapsed batch =\n%s", collapsed)
	}
	group := model.timeline[len(model.timeline)-1].web
	model.refreshViewport()
	row := -1
	for index, line := range selectionLines(model.viewport.GetContent(), model.viewport.Width()) {
		if strings.HasPrefix(strings.TrimSpace(line.text), "▸ … +") {
			row = index
			break
		}
	}
	visibleRow := row - model.viewport.YOffset()
	if row < 0 || visibleRow < 0 || visibleRow >= model.viewport.Height() {
		t.Fatalf("disclosure row=%d offset=%d height=%d", row, model.viewport.YOffset(), model.viewport.Height())
	}
	_, _ = model.handleMouse(tea.MouseClickMsg{X: model.viewportLeftInset() + 3, Y: model.layout().viewportTop + visibleRow, Button: tea.MouseLeft})
	if !group.expanded {
		t.Fatalf("disclosure click did not expand the group at row=%d", row)
	}
	expanded := ansi.Strip(model.renderTimeline())
	if !strings.Contains(expanded, "query-1") || !strings.Contains(expanded, "query-6") || !strings.Contains(expanded, "▾ … 6 shown") {
		t.Fatalf("expanded batch =\n%s", expanded)
	}
}

func TestWebActivitySpinnerTickRefreshesViewport(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoColor: true, Width: 80, Height: 24})
	model.operationID = "op-web"
	model.state = StateStreaming
	activity := protocol.WebActivity{CallID: "web-1", Kind: protocol.ToolWebSearch, Status: protocol.WebStatusRunning}
	_, _ = model.handleBackendEvent(Event{OperationID: "op-web", Kind: EventWebActivity, WebActivity: &activity})
	before := model.viewport.GetContent()
	_, _ = model.Update(operationSpinnerMsg{operationID: "op-web", message: model.spinner.Tick()})
	after := model.viewport.GetContent()
	if before == after {
		t.Fatal("running Web activity did not refresh with the spinner tick")
	}
}

func TestToolCompletionIgnoresActiveCardsBeforeCurrentTimelineGroup(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	model.operationID = "op-current"
	model.timeline = []timelineItem{
		{kind: timelineTool, tool: &toolView{callID: "stale", running: true}},
		{kind: timelineMessage, role: "user", text: "new request"},
		{kind: timelineTool, tool: &toolView{callID: "current", running: true}},
	}
	_, _ = model.handleBackendEvent(Event{OperationID: model.operationID, Kind: EventToolResult, ToolResult: &protocol.ToolResult{CallID: "current", Content: "done"}})
	if model.state != StateWaitingFirstToken {
		t.Fatalf("state=%s", model.state)
	}
}

func TestApprovalRejectReasonAndImmediateInterruptDecisions(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	model.operationID = "op-approval"
	model.eventChannel = make(chan Event, 1)
	response := make(chan ApprovalDecision, 1)
	model.approval = &ApprovalRequest{Tool: "bash", Reason: "Run focused tests", Response: response}
	model.state = StateAwaitingApproval

	_, _ = model.handleApprovalKey("tab")
	if !model.approvalEditing || model.approvalCursor != 1 {
		t.Fatalf("editing=%t cursor=%d", model.approvalEditing, model.approvalCursor)
	}
	_, _ = model.Update(tea.PasteMsg{Content: "Use the existing test target"})
	if model.approvalReason.Value() != "Use the existing test target" {
		t.Fatalf("pasted rejection reason = %q", model.approvalReason.Value())
	}
	_, _ = model.handleApprovalKey("enter")
	if decision := <-response; decision.Approved || decision.Reason != "Use the existing test target" {
		t.Fatalf("decision=%#v", decision)
	}

	response = make(chan ApprovalDecision, 1)
	model.approval = &ApprovalRequest{Tool: "bash", Reason: "Run focused tests", Response: response}
	model.state = StateAwaitingApproval
	_, _ = model.handleApprovalKey("n")
	if decision := <-response; decision.Approved || decision.Reason != "" {
		t.Fatalf("decision=%#v", decision)
	}

	compact := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 40, Height: 12})
	compact.approval = &ApprovalRequest{Tool: "bash", Risk: "exec", Summary: "$ go test ./...", Reason: strings.Repeat("Verify safely ", 20), PolicyReason: "manual confirmation", Step: 1, Total: 1}
	view := ansi.Strip(compact.View().Content)
	if lipgloss.Height(view) > 12 || !strings.Contains(view, "Why") || !strings.Contains(view, "Yes") || !strings.Contains(view, "No") {
		t.Fatalf("compact approval height=%d\n%s", lipgloss.Height(view), view)
	}
	for _, line := range strings.Split(view, "\n") {
		if lipgloss.Width(line) > 40 {
			t.Fatalf("compact approval width=%d line=%q", lipgloss.Width(line), line)
		}
	}

	visible := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	visible.timeline = []timelineItem{{kind: timelineMessage, role: "agent", text: "APPROVAL_HISTORY_SENTINEL"}}
	visible.approval = &ApprovalRequest{Tool: "bash", Risk: "exec", Summary: "$ go test ./...", Reason: "Verify the change", PolicyReason: "manual confirmation", Step: 1, Total: 1}
	visible.updateViewportHeight()
	visible.refreshViewport()
	approvalView := ansi.Strip(visible.View().Content)
	approvalRow := -1
	for row, line := range strings.Split(approvalView, "\n") {
		if strings.Contains(line, "Action approval") {
			approvalRow = row
			break
		}
	}
	if !strings.Contains(approvalView, "APPROVAL_HISTORY_SENTINEL") || approvalRow < visible.height*2/3 || lipgloss.Height(approvalView) != visible.height {
		t.Fatalf("approval history/layout: row=%d height=%d\n%s", approvalRow, lipgloss.Height(approvalView), approvalView)
	}
}

func TestAskWorkbenchSingleMultipleCustomBackAndCancel(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	model.operationID = "op-ask"
	model.eventChannel = make(chan Event, 4)
	response := make(chan AskDecision, 1)
	questions := []protocol.AskQuestion{
		{ID: "scope", Header: "Scope", Question: "Choose implementation scope", Options: []protocol.AskOption{{Label: "Small", Description: "Focused change"}, {Label: "Full", Description: "Complete flow"}}},
		{ID: "checks", Header: "Checks", Question: "Choose verification", Multiple: true, Options: []protocol.AskOption{{Label: "Unit", Description: "Focused tests"}, {Label: "Vet", Description: "Static checks"}, {Label: "Smoke", Description: "Interactive smoke"}}},
	}
	_, _ = model.handleBackendEvent(Event{OperationID: "op-ask", Kind: EventAsk, Ask: &AskRequest{Questions: questions, Response: response}})
	if model.state != StateAwaitingInput || model.ask == nil || !strings.Contains(ansi.Strip(model.View().Content), "Choose implementation scope") {
		t.Fatalf("state=%s ask=%#v\n%s", model.state, model.ask, ansi.Strip(model.View().Content))
	}
	compact := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 40, Height: 12})
	compact.snapshot.TodoList = protocol.TodoList{Items: []protocol.TodoItem{{ID: "hidden", Content: "Hidden while asking", Status: protocol.TodoInProgress}}}
	compact.ask = newAskState(&AskRequest{Questions: questions[:1]}, 40)
	compactView := ansi.Strip(compact.View().Content)
	if lipgloss.Height(compactView) != 12 || strings.Contains(compactView, "Hidden while asking") || strings.Contains(compactView, "Message Eylu") {
		t.Fatalf("compact ask layout:\n%s", compactView)
	}
	for _, line := range strings.Split(compactView, "\n") {
		if lipgloss.Width(line) > 40 {
			t.Fatalf("compact ask line width=%d line=%q", lipgloss.Width(line), line)
		}
	}
	_, _ = model.handleAskKey("down")
	_, _ = model.handleAskKey("enter")
	if model.ask.question != 1 {
		t.Fatalf("question index = %d", model.ask.question)
	}
	_, _ = model.handleAskKey("left")
	if model.ask.question != 0 {
		t.Fatalf("back question index = %d", model.ask.question)
	}
	_, _ = model.handleAskKey("right")
	_, _ = model.handleAskKey("space")
	_, _ = model.handleAskKey("tab")
	if !model.ask.editing {
		t.Fatal("custom input did not open")
	}
	_, _ = model.Update(tea.PasteMsg{Content: "custom detail"})
	_, command := model.handleAskKey("enter")
	if command == nil || model.ask != nil {
		t.Fatalf("command=%v ask=%#v", command, model.ask)
	}
	decision := <-response
	if got := decision.Answers["scope"]; len(got) != 1 || got[0] != "Full" {
		t.Fatalf("scope=%#v", got)
	}
	if got := decision.Answers["checks"]; len(got) != 2 || got[0] != "Unit" || got[1] != "custom detail" {
		t.Fatalf("checks=%#v", got)
	}

	cancelResponse := make(chan AskDecision, 1)
	_, _ = model.handleBackendEvent(Event{OperationID: "op-ask", Kind: EventAsk, Ask: &AskRequest{Questions: questions[:1], Response: cancelResponse}})
	_, cancelCommand := model.handleAskKey("esc")
	if decision := <-cancelResponse; !decision.Cancelled || cancelCommand == nil || model.ask != nil {
		t.Fatalf("cancel decision=%#v command=%v ask=%#v", decision, cancelCommand, model.ask)
	}

	interruptResponse := make(chan AskDecision, 1)
	model.ask = newAskState(&AskRequest{Questions: questions[:1], Response: interruptResponse}, model.width)
	model.state = StateAwaitingInput
	model.cancel = func() {}
	_, interruptCommand := model.handleInterrupt()
	if decision := <-interruptResponse; !decision.Cancelled || interruptCommand == nil || model.state != StateCancelling {
		t.Fatalf("interrupt decision=%#v command=%v state=%s", decision, interruptCommand, model.state)
	}
}

func TestTaskPanelTasksScreenAndHiddenTodoTimelineAcrossLayouts(t *testing.T) {
	list := protocol.TodoList{Items: []protocol.TodoItem{
		{ID: "done_one", Content: "Completed one", Status: protocol.TodoCompleted},
		{ID: "pending_one", Content: "Pending one", Status: protocol.TodoPending},
		{ID: "done_two", Content: "Completed two", Status: protocol.TodoCompleted},
		{ID: "active", Content: "Implement workbench", Status: protocol.TodoInProgress},
		{ID: "pending_two", Content: "Pending two", Status: protocol.TodoPending},
		{ID: "done_three", Content: "Completed three", Status: protocol.TodoCompleted},
		{ID: "pending_three", Content: "Pending three", Status: protocol.TodoPending},
		{ID: "pending_four", Content: "Pending four", Status: protocol.TodoPending},
	}}
	for _, size := range []struct{ width, height int }{{40, 12}, {80, 24}} {
		model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: size.width, Height: size.height})
		model.snapshot.TodoList = list
		_ = model.input.Focus()
		model.input.SetValue("message")
		model.refreshViewport()
		view := model.View()
		plain := ansi.Strip(view.Content)
		if !strings.Contains(plain, "Implement workbench") {
			t.Fatalf("%dx%d task panel missing:\n%s", size.width, size.height, plain)
		}
		for _, line := range strings.Split(plain, "\n") {
			if strings.Contains(line, "Implement workbench") {
				left := lipgloss.Width(line) - lipgloss.Width(strings.TrimLeft(line, " "))
				want := model.viewportLeftInset() + 2
				if left != want {
					t.Fatalf("%dx%d task inset=%d want=%d line=%q", size.width, size.height, left, want, line)
				}
			}
		}
		if size.width >= 80 {
			for _, expected := range []string{"Pending one", "Pending two", "Pending three", "Pending four", "... +3 completed"} {
				if !strings.Contains(plain, expected) {
					t.Fatalf("wide task panel missing %q:\n%s", expected, plain)
				}
			}
			for _, completed := range []string{"Completed one", "Completed two", "Completed three"} {
				if strings.Contains(plain, completed) {
					t.Fatalf("completed overflow item %q remained visible:\n%s", completed, plain)
				}
			}
			if model.layout().taskRows != 0 {
				t.Fatalf("wide task rows=%d layout=%#v", model.layout().taskRows, model.layout())
			}
		}
		for _, line := range strings.Split(plain, "\n") {
			if lipgloss.Width(line) > size.width {
				t.Fatalf("%dx%d line width=%d line=%q", size.width, size.height, lipgloss.Width(line), line)
			}
		}
		if view.Cursor == nil || view.Cursor.Position.Y != model.layout().inputContentRow {
			t.Fatalf("%dx%d cursor=%#v layout=%#v", size.width, size.height, view.Cursor, model.layout())
		}
	}
	pendingItems := []protocol.TodoItem{{ID: "active", Content: "Active task", Status: protocol.TodoInProgress}}
	for index := 1; index <= 11; index++ {
		pendingItems = append(pendingItems, protocol.TodoItem{ID: fmt.Sprintf("pending_%d", index), Content: fmt.Sprintf("Pending task %d", index), Status: protocol.TodoPending})
	}
	pendingModel := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	pendingModel.snapshot.TodoList = protocol.TodoList{Items: pendingItems}
	pendingModel.state = StateWaitingFirstToken
	pendingPanel := ansi.Strip(pendingModel.renderTaskPanel(6))
	if lipgloss.Height(pendingPanel) != 6 || !strings.Contains(pendingPanel, "... +7 pending") || pendingModel.taskPanelRows() != 6 {
		t.Fatalf("pending overflow panel:\n%s", pendingPanel)
	}
	completedModel := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	completedModel.snapshot.TodoList = protocol.TodoList{Items: []protocol.TodoItem{
		{ID: "active", Content: "Task A", Status: protocol.TodoInProgress},
		{ID: "open_one", Content: "Task B", Status: protocol.TodoPending},
		{ID: "open_two", Content: "Task C", Status: protocol.TodoPending},
	}}
	completedModel.timeline = []timelineItem{{kind: timelineNotice, text: "Completed in 25s."}}
	completedModel.state = StateCompleted
	_ = completedModel.input.Focus()
	completedModel.refreshViewport()
	completedView := ansi.Strip(completedModel.View().Content)
	completedRow, summaryRow := -1, -1
	for row, line := range strings.Split(completedView, "\n") {
		if strings.Contains(line, "Completed in 25s.") {
			completedRow = row
		}
		if strings.Contains(line, "3 tasks (0 done, 1 in progress, 2 open)") {
			summaryRow = row
		}
	}
	if completedRow < 0 || summaryRow != completedRow+2 {
		t.Fatalf("completed/task spacing completed_row=%d summary_row=%d:\n%s", completedRow, summaryRow, completedView)
	}

	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	model.snapshot.TodoList = list
	_, _ = model.executeSlash("/tasks")
	if model.screen != screenTasks {
		t.Fatalf("screen = %s", model.screen)
	}
	model.refreshViewport()
	for _, expected := range []string{"[>] Implement workbench", "[ ] Pending one", "[x] Completed one"} {
		if !strings.Contains(model.viewport.GetContent(), expected) {
			t.Fatalf("tasks screen missing %q:\n%s", expected, model.viewport.GetContent())
		}
	}
	if strings.Index(model.viewport.GetContent(), "Completed one") < strings.Index(model.viewport.GetContent(), "Pending four") {
		t.Fatalf("completed tasks were not moved to the end:\n%s", model.viewport.GetContent())
	}

	model.screen = screenChat
	model.operationID = "op-todo"
	model.eventChannel = make(chan Event, 2)
	call := protocol.ToolCall{ID: "call-todo", Name: "todolist", Arguments: []byte(`{"items":[]}`)}
	_, _ = model.handleBackendEvent(Event{OperationID: "op-todo", Kind: EventToolStart, ToolCall: &call})
	result := protocol.ToolResult{CallID: "call-todo", Content: "details", TodoList: &list}
	_, _ = model.handleBackendEvent(Event{OperationID: "op-todo", Kind: EventToolResult, ToolResult: &result})
	if len(model.snapshot.TodoList.Items) != 8 || strings.Contains(ansi.Strip(model.renderTimeline()), "todolist") || model.timeline[len(model.timeline)-1].tool.todoList == nil {
		t.Fatalf("snapshot=%#v tool=%#v", model.snapshot.TodoList, model.timeline[len(model.timeline)-1].tool)
	}
}

func TestMarkdownInlineCodeUsesThemeColorWithoutBackground(t *testing.T) {
	style := eyluMarkdownStyle()
	if style.Code.Color == nil || *style.Code.Color != eyluAccentColor {
		t.Fatalf("inline code color=%v", style.Code.Color)
	}
	if style.Code.BackgroundColor != nil {
		t.Fatalf("inline code background=%q", *style.Code.BackgroundColor)
	}
}

func TestPlanCompletionGateAutoFullRejectAndFeedback(t *testing.T) {
	backend := &fakeBackend{}
	model := NewModel(backend, Options{NoAnimation: true, NoColor: true})
	model.snapshot.Mode = "plan"
	model.operationMode = "plan"
	model.operationID = "op-plan"
	model.state = StateCompleted
	_, _ = model.Update(backendEventClosedMsg{operationID: "op-plan"})
	if model.planGate == nil || !strings.Contains(ansi.Strip(model.View().Content), "Start implementation") {
		t.Fatalf("plan gate=%#v view=%s", model.planGate, ansi.Strip(model.View().Content))
	}

	_, modeCommand := model.handlePlanGateKey("enter")
	if modeCommand == nil || model.pendingImplementationMode != "auto" {
		t.Fatalf("pending=%q command=%v", model.pendingImplementationMode, modeCommand)
	}
	_, startCommand := model.Update(modeCommand())
	if model.state != StateConnecting || model.operationMode != "auto" || model.draft != approvedPlanImplementationPrompt || startCommand == nil {
		t.Fatalf("state=%s mode=%q draft=%q command=%v", model.state, model.operationMode, model.draft, startCommand)
	}

	model.state = StateCompleted
	model.planGate = newPlanGate(model.width)
	model.planGate.cursor = 1
	_, fullCommand := model.handlePlanGateKey("enter")
	if fullCommand == nil || model.pendingImplementationMode != "full" {
		t.Fatalf("pending=%q command=%v", model.pendingImplementationMode, fullCommand)
	}
	backend.err = errors.New("persist full failed")
	_, _ = model.Update(fullCommand())
	if model.planGate == nil || model.pendingImplementationMode != "" {
		t.Fatalf("mode failure gate=%#v pending=%q", model.planGate, model.pendingImplementationMode)
	}
	backend.err = nil

	model.pendingImplementationMode = ""
	model.planGate = newPlanGate(model.width)
	model.planGate.cursor = 2
	_, rejectCommand := model.handlePlanGateKey("enter")
	if rejectCommand == nil || model.planGate != nil || model.snapshot.Mode != "manual" {
		t.Fatalf("gate=%#v mode=%q command=%v", model.planGate, model.snapshot.Mode, rejectCommand)
	}

	model.snapshot.Mode = "plan"
	model.planGate = newPlanGate(model.width)
	_, _ = model.handlePlanGateKey("tab")
	model.planGate.feedback.SetValue("Keep the public API stable")
	_, feedbackCommand := model.handlePlanGateKey("enter")
	if feedbackCommand == nil || model.state != StateConnecting || !strings.Contains(model.draft, "Keep the public API stable") || model.operationMode != "plan" || len(model.promptHistory) != 1 || model.promptHistory[0] != "Keep the public API stable" {
		t.Fatalf("state=%s mode=%q draft=%q command=%v", model.state, model.operationMode, model.draft, feedbackCommand)
	}

	compact := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 40, Height: 12})
	compact.planGate = newPlanGate(40)
	view := ansi.Strip(compact.View().Content)
	if lipgloss.Height(view) > 12 || !strings.Contains(view, "Auto") || !strings.Contains(view, "Full") || !strings.Contains(view, "Reject") {
		t.Fatalf("compact plan gate height=%d\n%s", lipgloss.Height(view), view)
	}

	visible := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	visible.timeline = []timelineItem{{kind: timelineMessage, role: "agent", text: "PLAN_HISTORY_SENTINEL\n\n1. Inspect\n2. Implement\n3. Verify"}}
	visible.planGate = newPlanGate(80)
	visible.updateViewportHeight()
	visible.refreshViewport()
	planView := ansi.Strip(visible.View().Content)
	planLines := strings.Split(planView, "\n")
	gateRow := -1
	for row, line := range planLines {
		if strings.Contains(line, "Start implementation") {
			gateRow = row
			break
		}
	}
	if !strings.Contains(planView, "PLAN_HISTORY_SENTINEL") || gateRow < visible.height*2/3 || lipgloss.Height(planView) != visible.height {
		t.Fatalf("plan history/gate layout: gate_row=%d height=%d\n%s", gateRow, lipgloss.Height(planView), planView)
	}
}

func TestStreamingActivityShowsEstimatedExactTokensAndThinking(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 19, 12, 0, 14, 0, time.UTC)}
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Clock: clock, Width: 100, Height: 24})
	model.operationID = "op-activity"
	model.eventChannel = make(chan Event, 8)
	model.startedAt = clock.now.Add(-14 * time.Second)

	_, _ = model.handleBackendEvent(Event{OperationID: "op-activity", Kind: EventActivity, Activity: &Activity{Reasoning: true, ReasoningKnown: true, TokenBytesPerToken: 4, InputTokens: 1200}})
	_, _ = model.handleBackendEvent(Event{OperationID: "op-activity", Kind: EventState, State: StateWaitingFirstToken})
	_, _ = model.handleBackendEvent(Event{OperationID: "op-activity", Kind: EventReasoningDelta, Delta: strings.Repeat("r", 204)})
	thinking := ansi.Strip(model.renderLoading())
	if !strings.Contains(thinking, "Thinking...") || !strings.Contains(thinking, " · thinking)") || strings.Contains(thinking, "thinking ≈") || strings.Contains(thinking, "thinking 320") {
		t.Fatalf("active reasoning = %q", thinking)
	}
	clock.now = clock.now.Add(13 * time.Second)
	_, _ = model.handleBackendEvent(Event{OperationID: "op-activity", Kind: EventTextDelta, Delta: strings.Repeat("x", 2936)})
	estimated := ansi.Strip(model.renderLoading())
	for _, expected := range []string{"Composing...", "27s", "↑ ≈1200 sent", "↓ ≈734 received", "thought for 13s"} {
		if !strings.Contains(estimated, expected) {
			t.Fatalf("estimated activity missing %q: %q", expected, estimated)
		}
	}

	usage := protocol.Usage{InputTokens: 1100, OutputTokens: 700, ReasoningTokens: 320, Exact: true}
	_, _ = model.handleBackendEvent(Event{OperationID: "op-activity", Kind: EventUsage, Usage: &usage})
	exact := ansi.Strip(model.renderLoading())
	for _, expected := range []string{"↑ 1100 sent", "↓ 700 received", "thought for 13s"} {
		if !strings.Contains(exact, expected) {
			t.Fatalf("exact activity missing %q: %q", expected, exact)
		}
	}
	if strings.Contains(exact, "≈") {
		t.Fatalf("exact activity remained estimated: %q", exact)
	}
	model.state = StateExecutingTool
	if toolActivity := ansi.Strip(model.renderLoading()); !strings.Contains(toolActivity, "thought for 13s") {
		t.Fatalf("tool activity lost completed thought duration: %q", toolActivity)
	}

	_, _ = model.handleBackendEvent(Event{OperationID: "op-activity", Kind: EventState, State: StateWaitingFirstToken})
	_, _ = model.handleBackendEvent(Event{OperationID: "op-activity", Kind: EventActivity, Activity: &Activity{InputTokens: 1500, TokenBytesPerToken: 4}})
	pending := ansi.Strip(model.renderLoading())
	for _, expected := range []string{"Thinking...", "↑ ≈1500 sent"} {
		if !strings.Contains(pending, expected) {
			t.Fatalf("pending activity missing %q: %q", expected, pending)
		}
	}
	if strings.Contains(pending, "thinking") || strings.Contains(pending, "thought for") {
		t.Fatalf("pending activity reused a prior-round thought: %q", pending)
	}
	_, _ = model.handleBackendEvent(Event{OperationID: "op-activity", Kind: EventReasoningDelta, Delta: strings.Repeat("z", 40)})
	clock.now = clock.now.Add(2 * time.Second)
	_, _ = model.handleBackendEvent(Event{OperationID: "op-activity", Kind: EventTextDelta, Delta: strings.Repeat("y", 40)})
	nextTurn := ansi.Strip(model.renderLoading())
	for _, expected := range []string{"↓ ≈710 received", "thought for 2s"} {
		if !strings.Contains(nextTurn, expected) {
			t.Fatalf("next turn activity missing %q: %q", expected, nextTurn)
		}
	}
	inexactUsage := protocol.Usage{}
	_, _ = model.handleBackendEvent(Event{OperationID: "op-activity", Kind: EventUsage, Usage: &inexactUsage})
	if local := ansi.Strip(model.renderLoading()); !strings.Contains(local, "thought for 2s") {
		t.Fatalf("local thought duration was lost without exact usage: %q", local)
	}

	model.resize(40, 16)
	if width := lipgloss.Width(ansi.Strip(model.renderLoading())); width > 40 {
		t.Fatalf("activity width=%d line=%q", width, ansi.Strip(model.renderLoading()))
	}
}

func TestCompactingActivityShowsOnlyElapsedAndPreservesTaskHint(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 20, 12, 0, 1, 0, time.UTC)}
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Clock: clock, Width: 100, Height: 24})
	model.state = StateCompacting
	model.startedAt = clock.now.Add(-time.Second)
	model.activity = Activity{InputTokens: 3791, InputExact: true, TokenBytesPerToken: 4}
	model.operationUsage = protocol.Usage{OutputTokens: 20, Exact: true}
	model.reasoningSeen = true
	model.reasoningElapsed = 10 * time.Second
	activity := strings.TrimSpace(ansi.Strip(model.renderLoading()))
	if activity != "* Compacting...  (1s)" {
		t.Fatalf("activity=%q", activity)
	}
	if status := ansi.Strip(model.renderStatus()); !strings.Contains(status, "Preserving task context") {
		t.Fatalf("status=%q", status)
	}

	fresh := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Clock: clock, Width: 100, Height: 24})
	started, command := fresh.executeSlash("/compact")
	compactModel := started.(*Model)
	if compactModel.state != StateCompacting || command == nil || len(compactModel.timeline) != 0 {
		t.Fatalf("state=%s command=%v timeline=%#v", compactModel.state, command, compactModel.timeline)
	}
	items := slashCompletionItems("/comp", Snapshot{})
	if len(items) != 1 || items[0].label != "/compact" {
		t.Fatalf("completion=%#v", items)
	}
}

func TestStatusAlignsWithInputAndUsesFriendlyContextCopy(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 100, Height: 24})
	model.contextStarted = true
	model.snapshot = Snapshot{Mode: "manual", Context: contextledger.Report{TotalTokens: 2_400, ContextWindow: 100_000, LimitKnown: true, Percent: 2.4}}
	model.state = StateIdle
	status := ansi.Strip(model.renderStatus())
	if !strings.HasPrefix(status, "  manual · Context 98% left · Context 2% used") || !strings.HasSuffix(strings.TrimSpace(status), "Ready when you are") {
		t.Fatalf("status=%q", status)
	}
	if lipgloss.Width(status) != 100 {
		t.Fatalf("status width=%d", lipgloss.Width(status))
	}

	model.resize(40, 16)
	narrow := ansi.Strip(model.renderStatus())
	if !strings.HasPrefix(narrow, "  manual · 98% left · 2% used") || strings.Contains(narrow, "Ready when") {
		t.Fatalf("narrow status=%q", narrow)
	}

	model.resize(80, 24)
	model.snapshot.Context = contextledger.Report{TotalTokens: 12_400}
	unknown := ansi.Strip(model.renderStatus())
	if !strings.HasPrefix(unknown, "  manual · Context 12.4K tokens") {
		t.Fatalf("unknown status=%q", unknown)
	}

	model.snapshot.Context = contextledger.Report{TotalTokens: 120, ContextWindow: 100, LimitKnown: true, Percent: 120}
	overflow := ansi.Strip(model.renderStatus())
	if !strings.Contains(overflow, "Context 0% left · Context 100% used") {
		t.Fatalf("overflow status=%q", overflow)
	}
}

func TestPristineSessionShowsFullContextUntilFirstPrompt(t *testing.T) {
	report := contextledger.Report{
		Provider: "default", Model: "gpt-5.6-sol", ContextWindow: 200_000, LimitKnown: true,
		InputTokens: 4_000, OutputReserve: 8_000, TotalTokens: 12_000, Percent: 6,
		Categories: []contextledger.CategoryUsage{{Category: contextledger.CategorySystemPrompt, Label: "System prompt", Tokens: 4_000}},
	}
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	_, _ = model.Update(snapshotMsg{snapshot: Snapshot{SessionID: "fresh", Mode: "manual", Context: report}})
	status := ansi.Strip(model.renderStatus())
	if !strings.Contains(status, "Context 100% left · Context 0% used") {
		t.Fatalf("fresh status=%q", status)
	}
	contextView := model.renderContext()
	if !strings.Contains(contextView, "0% used") || !strings.Contains(contextView, "200K free") || strings.Contains(contextView, "System") {
		t.Fatalf("fresh context view:\n%s", contextView)
	}

	_, _ = model.startRequest(Submission{Text: "hello", HistoryText: "hello"})
	if status = ansi.Strip(model.renderStatus()); !strings.Contains(status, "Context 94% left · Context 6% used") {
		t.Fatalf("started status=%q", status)
	}

	_, _ = model.Update(snapshotMsg{snapshot: Snapshot{SessionID: "new-session", Mode: "manual", Context: report}})
	if status = ansi.Strip(model.renderStatus()); !strings.Contains(status, "Context 100% left · Context 0% used") {
		t.Fatalf("new session status=%q", status)
	}
}

func TestBannerUsesSpacedItalicWordmarkAndMiddleTruncatedWorkspace(t *testing.T) {
	workspace := "E:/Projects/very-long-parent-directory/another-long-directory/Eylu"
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Version: "1.2.3", Workspace: workspace, Width: 40, Height: 20})
	banner := ansi.Strip(model.renderBanner())
	for _, expected := range []string{"______   __  __", "/ /___", "v1.2.3", "E:/", "Eylu"} {
		if !strings.Contains(banner, expected) {
			t.Fatalf("banner missing %q:\n%s", expected, banner)
		}
	}
	for _, line := range strings.Split(banner, "\n") {
		if lipgloss.Width(line) > model.viewportContentWidth() {
			t.Fatalf("banner line width=%d line=%q", lipgloss.Width(line), line)
		}
	}
	if count := strings.Count(ansi.Strip(model.renderTimeline()), "______"); count != 1 {
		t.Fatalf("banner count=%d", count)
	}

	styled := NewModel(&fakeBackend{}, Options{NoAnimation: true, Version: "1.2.3", Workspace: workspace, Width: 80, Height: 24})
	if rendered := styled.renderBanner(); !strings.Contains(rendered, "\x1b[1;") {
		t.Fatalf("banner is not bold: %q", rendered)
	}
}

func TestThemeGradientUsesDisplayColumnsAndPreservesText(t *testing.T) {
	value := "A界 B\nCD"
	elapsed := 1250 * time.Millisecond
	rendered := renderThemeGradient(value, elapsed, false)
	if plain := ansi.Strip(rendered); plain != value {
		t.Fatalf("plain=%q want=%q", plain, value)
	}
	if count := strings.Count(rendered, "\x1b[38;2;"); count != 6 {
		t.Fatalf("true-color sequence count=%d rendered=%q", count, rendered)
	}
	for index, line := range strings.Split(rendered, "\n") {
		if got, want := lipgloss.Width(line), lipgloss.Width(strings.Split(value, "\n")[index]); got != want {
			t.Fatalf("line %d width=%d want=%d rendered=%q", index, got, want, line)
		}
	}
	first := themeGradientRGB(0, elapsed)
	adjacent := themeGradientRGB(1, elapsed)
	afterWideRune := themeGradientRGB(3, elapsed)
	later := themeGradientRGB(0, elapsed+colorAnimationInterval)
	if first == adjacent || adjacent == afterWideRune {
		t.Fatalf("display columns did not produce a gradient: first=%v adjacent=%v wide=%v", first, adjacent, afterWideRune)
	}
	if first == later {
		t.Fatalf("time did not advance the gradient: first=%v later=%v", first, later)
	}
	for _, rgb := range [][3]uint8{first, adjacent, afterWideRune, later} {
		for channel := range rgb {
			if rgb[channel] > eyluAccentRGB[channel] {
				t.Fatalf("theme gradient exceeded accent color: rgb=%v accent=%v", rgb, eyluAccentRGB)
			}
		}
	}
}

func TestColorAnimationTickRecolorsBannerStatusAndSkipsHiddenBannerRefresh(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)}
	model := NewModel(&fakeBackend{}, Options{Clock: clock, Version: "1.2.3", Workspace: "E:/Eylu", Width: 80, Height: 24})
	if model.colorAnimationTickCmd() != nil {
		t.Fatal("default-disabled gradient scheduled a color tick")
	}
	_, enableCommand := model.Update(snapshotMsg{snapshot: Snapshot{Mode: "manual", GradientEnabled: true}})
	if enableCommand == nil {
		t.Fatal("enabled gradient did not schedule a color tick")
	}
	generation := model.colorAnimationGeneration
	model.followOutput = false
	model.timeline = []timelineItem{{kind: timelineMessage, role: "user", text: strings.Repeat("history line\n", 40)}}
	model.refreshViewport()
	model.viewport.GotoTop()
	scheduled, ok := model.colorAnimationTickCmd()().(colorAnimationTickMsg)
	if !ok || scheduled.at.Sub(clock.now) != 50*time.Millisecond {
		t.Fatalf("scheduled tick=%#v", scheduled)
	}

	bannerBefore := model.renderBanner()
	statusBefore := model.renderStatus()
	viewportBefore := model.viewport.GetContent()
	_, command := model.Update(colorAnimationTickMsg{at: clock.now.Add(colorAnimationInterval), generation: generation})
	if command == nil {
		t.Fatal("color animation tick did not reschedule")
	}
	if model.colorAnimationElapsed != colorAnimationInterval {
		t.Fatalf("elapsed=%s want=%s", model.colorAnimationElapsed, colorAnimationInterval)
	}
	bannerAfter := model.renderBanner()
	statusAfter := model.renderStatus()
	if bannerBefore == bannerAfter || statusBefore == statusAfter {
		t.Fatalf("tick did not recolor banner/status: banner_changed=%t status_changed=%t", bannerBefore != bannerAfter, statusBefore != statusAfter)
	}
	if ansi.Strip(bannerBefore) != ansi.Strip(bannerAfter) || ansi.Strip(statusBefore) != ansi.Strip(statusAfter) {
		t.Fatal("animation changed visible text")
	}
	if viewportBefore == model.viewport.GetContent() {
		t.Fatal("visible banner viewport was not refreshed")
	}

	model.viewport.SetYOffset(bannerViewportRows)
	hiddenBefore := model.viewport.GetContent()
	_, _ = model.Update(colorAnimationTickMsg{at: clock.now.Add(2 * colorAnimationInterval), generation: generation})
	if hiddenBefore != model.viewport.GetContent() {
		t.Fatal("hidden banner caused a full viewport refresh")
	}

	_, _ = model.Update(snapshotMsg{snapshot: Snapshot{Mode: "manual", GradientEnabled: false}})
	disabledElapsed := model.colorAnimationElapsed
	_, staleCommand := model.Update(colorAnimationTickMsg{at: clock.now.Add(3 * colorAnimationInterval), generation: generation})
	if staleCommand != nil || model.colorAnimationElapsed != disabledElapsed {
		t.Fatal("disabled gradient accepted a stale tick")
	}
	_, reenabledCommand := model.Update(snapshotMsg{snapshot: Snapshot{Mode: "manual", GradientEnabled: true}})
	if reenabledCommand == nil || model.colorAnimationGeneration == generation {
		t.Fatal("re-enabled gradient did not start a fresh tick generation")
	}
}

func TestColorAnimationFallbacksKeepStaticStylesAndPlainText(t *testing.T) {
	static := NewModel(&fakeBackend{}, Options{NoAnimation: true, Version: "1.2.3", Workspace: "E:/Eylu", Width: 80, Height: 24})
	static.snapshot = Snapshot{Mode: "manual", GradientEnabled: true}
	static.gradientEnabled = true
	if static.colorAnimationTickCmd() != nil {
		t.Fatal("--no-animation scheduled a color tick")
	}
	if count := strings.Count(static.renderBanner(), "\x1b[38;2;"); count > 2 {
		t.Fatalf("--no-animation did not retain fixed banner styles: sequences=%d", count)
	}

	plain := NewModel(&fakeBackend{}, Options{NoColor: true, Version: "1.2.3", Workspace: "E:/Eylu", Width: 80, Height: 24})
	plain.snapshot = Snapshot{Mode: "manual", GradientEnabled: true}
	plain.gradientEnabled = true
	if plain.colorAnimationTickCmd() != nil {
		t.Fatal("NO_COLOR scheduled a color tick")
	}
	view := plain.View().Content
	if strings.Contains(view, "\x1b[") {
		t.Fatalf("NO_COLOR emitted ANSI: %q", view)
	}
}

func TestTimelineAddsOneBlankLineAfterVisibleToolGroup(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	model.timeline = []timelineItem{
		{kind: timelineTool, tool: &toolView{name: "read_file"}},
		{kind: timelineTool, tool: &toolView{name: "todolist"}},
		{kind: timelineTool, tool: &toolView{name: "bash"}},
		{kind: timelineMessage, role: "agent", text: "Tool result explained."},
	}
	rendered := ansi.Strip(model.renderTimeline())
	if strings.Contains(rendered, "> read_file  done\n\n> bash  done") {
		t.Fatalf("tool group contains an extra blank line:\n%s", rendered)
	}
	if !strings.Contains(rendered, "> bash  done\n\nEYLU\nTool result explained.") {
		t.Fatalf("tool group/message spacing is incorrect:\n%s", rendered)
	}
}

func TestContextMapGroupsUsageAndExpandsDetails(t *testing.T) {
	report := contextledger.Report{
		Provider: "default", Model: "gpt-5.6-sol", ConfiguredContextWindow: 200_000, DetectedContextWindow: 200_000,
		ContextWindow: 200_000, LimitKnown: true, LimitSource: "models_dev", LimitCached: true,
		InputTokens: 12_000, OutputReserve: 8_000, TotalTokens: 20_000, Percent: 10, CompressionCount: 1,
		LastCompression: &contextledger.CompressionEvent{BeforeTokens: 40_000, AfterTokens: 12_000, OmittedTurns: 4},
		LastUsage:       protocol.Usage{InputTokens: 9_000, OutputTokens: 1_200},
		Categories: []contextledger.CategoryUsage{
			{Category: contextledger.CategorySystemPrompt, Label: "System prompt", Tokens: 2_000, Measurement: "estimated", Sources: []contextledger.SourceUsage{{Source: "eylu", Tokens: 2_000}}},
			{Category: contextledger.CategoryUserMessage, Label: "User messages", Tokens: 3_000, Measurement: "estimated"},
			{Category: contextledger.CategoryBuiltinToolSchema, Label: "Tool schemas", Tokens: 4_000, Measurement: "estimated"},
			{Category: contextledger.CategorySkillBody, Label: "Skill body", Tokens: 1_000, Measurement: "estimated"},
			{Category: contextledger.CategoryMCPToolSchema, Label: "MCP tool schemas", Tokens: 2_000, Measurement: "estimated"},
			{Category: contextledger.CategoryOutputReserve, Label: "Output reserve", Tokens: 8_000, Measurement: "estimated"},
		},
	}
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	model.contextStarted = true
	model.snapshot.Context = report
	compact := model.renderContext()
	for _, expected := range []string{"Context map", "default · gpt-5.6-sol", "10% used", "◆", "◇", "System", "Conversation", "Tools", "Skills", "MCP", "180K free"} {
		if !strings.Contains(compact, expected) {
			t.Fatalf("compact context missing %q:\n%s", expected, compact)
		}
	}
	if strings.Contains(compact, "Categories") {
		t.Fatalf("compact context unexpectedly expanded:\n%s", compact)
	}

	model.contextExpand = true
	expanded := model.renderContext()
	for _, expected := range []string{"Details", "Categories", "System prompt", "eylu", "Compression", "40K → 12K", "Provider usage", "9K input · 1.2K output"} {
		if !strings.Contains(expanded, expected) {
			t.Fatalf("expanded context missing %q:\n%s", expected, expanded)
		}
	}

	model.snapshot.Context = contextledger.Report{InputTokens: 12_400, TotalTokens: 12_400, Categories: []contextledger.CategoryUsage{{Category: contextledger.CategoryAgentMessage, Label: "Agent messages", Tokens: 12_400}}}
	model.contextExpand = false
	unknown := model.renderContext()
	if !strings.Contains(unknown, "Limit unknown · 12.4K tokens tracked") || strings.Contains(unknown, "% used") {
		t.Fatalf("unknown context:\n%s", unknown)
	}
}

func TestActivityLineKeepsOneBlankRowBelowLatestMessage(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 19, 12, 0, 1, 0, time.UTC)}
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Clock: clock, Width: 80, Height: 24})
	model.timeline = []timelineItem{
		{kind: timelineMessage, role: "agent", text: strings.Repeat("history line\n", 30)},
		{kind: timelineMessage, role: "user", text: "我喜欢你"},
	}
	model.state = StateWaitingFirstToken
	model.startedAt = clock.now.Add(-time.Second)
	model.refreshViewport()
	lines := strings.Split(model.View().Content, "\n")
	messageRow, activityRow := -1, -1
	for row, line := range lines {
		if strings.Contains(line, "我喜欢你") {
			messageRow = row
		}
		if strings.Contains(line, "Thinking...") {
			activityRow = row
		}
	}
	if messageRow < 0 || activityRow-messageRow != 2 {
		t.Fatalf("message_row=%d activity_row=%d\n%s", messageRow, activityRow, model.View().Content)
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
	viewLines := strings.Split(ansi.Strip(view.Content), "\n")
	if view.Cursor.Position.Y >= len(viewLines) || !strings.Contains(viewLines[view.Cursor.Position.Y], "html写一个你的自我介绍页面") {
		t.Fatalf("cursor row=%d does not match input row:\n%s", view.Cursor.Position.Y, ansi.Strip(view.Content))
	}
	model.approval = &ApprovalRequest{Tool: "write_file", Risk: "write"}
	if cursor := model.View().Cursor; cursor != nil {
		t.Fatalf("approval cursor = %#v", cursor)
	}
}

func TestViewportContentStartsAtEmptyInputCursorColumnAndKeepsHeaderPosition(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	_ = model.input.Focus()
	model.timeline = []timelineItem{{kind: timelineMessage, role: "agent", text: "Aligned response"}}
	model.refreshViewport()

	view := model.View()
	if view.Cursor == nil {
		t.Fatal("empty input cursor is missing")
	}
	header := strings.Split(view.Content, "\n")[0]
	if !strings.HasPrefix(header, "Eylu") {
		t.Fatalf("header moved: %q", header)
	}
	assertInset := func(name string) {
		t.Helper()
		for _, line := range strings.Split(ansi.Strip(model.renderViewport()), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			left := lipgloss.Width(line) - lipgloss.Width(strings.TrimLeft(line, " "))
			if left != view.Cursor.Position.X {
				t.Fatalf("%s column=%d cursor column=%d line=%q", name, left, view.Cursor.Position.X, line)
			}
		}
	}
	timelineAligned := false
	for _, line := range strings.Split(ansi.Strip(model.renderViewport()), "\n") {
		if !strings.Contains(line, "Aligned response") {
			continue
		}
		left := lipgloss.Width(line) - lipgloss.Width(strings.TrimLeft(line, " "))
		if left != view.Cursor.Position.X {
			t.Fatalf("timeline column=%d cursor column=%d line=%q", left, view.Cursor.Position.X, line)
		}
		timelineAligned = true
	}
	if !timelineAligned {
		t.Fatal("aligned timeline response is missing")
	}

	model.screen = screenProviders
	model.snapshot.Providers = []ProviderItem{{Name: "work", Adapter: "openai_responses", Model: "model", Active: true}}
	model.refreshViewport()
	assertInset("providers")

	model.screen = screenProviderForm
	model.form = newProviderFormModel(ProviderForm{Name: "work", BaseURL: "https://example.com/v1", Model: "model", Adapter: "openai_responses"}, model.viewportContentWidth())
	model.refreshViewport()
	assertInset("provider form")

	model.screen = screenModels
	model.models = []string{"model"}
	model.refreshViewport()
	assertInset("models")

	model.screen = screenSkills
	model.snapshot.Skills = []SkillItem{{Name: "review", Source: "project", Status: "active", Activated: true}}
	model.refreshViewport()
	assertInset("skills")

	model.screen = screenContext
	model.refreshViewport()
	assertInset("context")

	model.screen = screenToolDetail
	model.timeline = []timelineItem{{kind: timelineTool, tool: &toolView{name: "read_file", arguments: `{"path":"README.md"}`, content: "ok"}}}
	model.toolCursor = 0
	model.refreshViewport()
	assertInset("tool detail")
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
	readResult := protocol.ToolResult{CallID: "read", Content: "one\ntwo", Metadata: map[string]any{"path": file, "bytes": int64(17), "lines": float64(2), "lines_complete": true}}
	model.timeline = []timelineItem{{kind: timelineTool, tool: &toolView{name: "read_file", callID: "read"}}}
	model.completeTool(&readResult)
	readRendered := ansi.Strip(model.renderTool(model.timeline[0].tool))
	if !strings.Contains(readRendered, "17 B") || !strings.Contains(readRendered, "2 lines") {
		t.Fatalf("read tool stats = %q tool=%#v", readRendered, model.timeline[0].tool)
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
	backend := &fakeBackend{submit: func(ctx context.Context, operationID string, _ Submission, emit func(Event)) error {
		for index := 0; index < 1024; index++ {
			emit(Event{OperationID: operationID, Kind: EventToolCallDelta, ToolCallDelta: &protocol.ToolCallDelta{ID: "call", Delta: "x"}})
		}
		return ctx.Err()
	}}
	events := make(chan Event, 1)
	done := make(chan tea.Msg, 1)
	go func() { done <- runBackendCmd(ctx, backend, "op-cancel", Submission{Text: "prompt"}, events)() }()
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
		Context:   contextledger.Report{InputTokens: 50, OutputReserve: 10, TotalTokens: 60, ContextWindow: 100, LimitKnown: true, Percent: 60, Categories: []contextledger.CategoryUsage{{Category: contextledger.CategoryUserMessage, Label: "User messages", Tokens: 50, Percent: 100, Measurement: "estimated"}}},
		Providers: []ProviderItem{{Name: "work", Adapter: "openai_responses", Model: "model", Active: true}},
		Skills:    []SkillItem{{Name: "demo-skill", Source: "project", Status: "active", Activated: true}},
	}}
	model := NewModel(backend, Options{NoAnimation: true, NoColor: true, Clock: clock, Width: 40, Height: 16})
	model.snapshot = backend.snapshot
	model.contextStarted = true
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
	if content := model.viewport.GetContent(); !strings.Contains(content, "Context map") || !strings.Contains(content, "◆") || !strings.Contains(content, "Conversation") || !strings.Contains(content, "Categories") {
		t.Fatalf("context panel = %q", content)
	}
}

func TestBackendCommandEventOrderingAndError(t *testing.T) {
	backend := &fakeBackend{submit: func(_ context.Context, operationID string, _ Submission, emit func(Event)) error {
		emit(Event{OperationID: operationID, Kind: EventState, State: StateWaitingFirstToken})
		emit(Event{OperationID: operationID, Kind: EventTextDelta, Delta: "done"})
		return nil
	}}
	events := make(chan Event, 8)
	message := runBackendCmd(context.Background(), backend, "op-1", Submission{Text: "hello"}, events)()
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
	_ = runBackendCmd(context.Background(), backend, "op-2", Submission{Text: "hello"}, events)()
	foundError := false
	for event := range events {
		foundError = foundError || event.Kind == EventNotice && event.Error
	}
	if !foundError {
		t.Fatal("backend error event was not marked")
	}

	backend.err = ErrRequestInterrupted
	events = make(chan Event, 4)
	_ = runBackendCmd(context.Background(), backend, "op-3", Submission{Text: "hello"}, events)()
	interrupted, errorNotice := false, false
	for event := range events {
		interrupted = interrupted || event.Kind == EventState && event.State == StateInterrupted
		errorNotice = errorNotice || event.Kind == EventNotice && event.Error
	}
	if !interrupted || errorNotice {
		t.Fatalf("interrupted=%t errorNotice=%t", interrupted, errorNotice)
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
	form.inputs[providerFieldCatalog].SetValue("openai")
	form.inputs[providerFieldContext].SetValue("32000")
	value, err := form.value()
	if err != nil || value.ContextWindow != 32000 || !value.ContextWindowSet || value.CatalogProvider != "openai" || !value.CatalogProviderSet || value.Model != "manual-model" {
		t.Fatalf("value=%#v err=%v", value, err)
	}
	edit := newProviderFormModel(ProviderForm{OriginalName: "work", Name: "work", BaseURL: "https://example.com/v1", Model: "model", Adapter: "openai_chat", CatalogProvider: "openai", ContextWindow: 32000}, 80)
	edit.inputs[providerFieldCatalog].SetValue("")
	edit.inputs[providerFieldContext].SetValue("0")
	edited, err := edit.value()
	if err != nil || !edited.CatalogProviderRemove || !edited.ContextWindowSet || edited.ContextWindow != 0 || edited.ContextWindowRemove {
		t.Fatalf("edited=%#v err=%v", edited, err)
	}
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true})
	model.models = []string{"alpha", "beta-code", "beta-chat"}
	model.modelFilter.SetValue("code")
	filtered := model.filteredModels()
	if len(filtered) != 1 || filtered[0] != "beta-code" {
		t.Fatalf("filtered = %#v", filtered)
	}
}

func TestModelsPaginationNavigationAndRendering(t *testing.T) {
	newPaginatedModel := func(width, height, count int) *Model {
		t.Helper()
		model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: width, Height: height})
		model.screen = screenModels
		model.state = StateIdle
		model.models = make([]string, count)
		for index := range model.models {
			model.models[index] = fmt.Sprintf("model-%02d", index)
		}
		return model
	}

	t.Run("renders only the selected responsive page", func(t *testing.T) {
		model := newPaginatedModel(120, 16, 18)
		firstPage := model.modelPage(len(model.models))
		if firstPage.size != max(1, model.viewport.Height()-4) {
			t.Fatalf("page size=%d viewport=%d", firstPage.size, model.viewport.Height())
		}
		model.modelCursor = firstPage.size + 2
		page := model.modelPage(len(model.models))
		rendered := ansi.Strip(model.renderModels())
		for index := page.start; index < page.end; index++ {
			if !strings.Contains(rendered, model.models[index]) {
				t.Fatalf("page item %q missing:\n%s", model.models[index], rendered)
			}
		}
		if page.start > 0 && strings.Contains(rendered, model.models[page.start-1]) {
			t.Fatalf("previous page item leaked into rendered page:\n%s", rendered)
		}
		footer := fmt.Sprintf("Page %d/%d · ←/→ page · ↑/↓ select · Enter use · m manual · r refresh · Esc back", page.number, page.total)
		if !strings.Contains(rendered, footer) {
			t.Fatalf("page footer missing %q:\n%s", footer, rendered)
		}
		view := ansi.Strip(model.View().Content)
		if lipgloss.Height(view) != model.height {
			t.Fatalf("view height=%d want=%d\n%s", lipgloss.Height(view), model.height, view)
		}
	})

	t.Run("left and right preserve the row when possible", func(t *testing.T) {
		model := newPaginatedModel(80, 16, 17)
		pageSize := model.modelPage(len(model.models)).size
		model.modelCursor = 2
		_, _ = model.handleModelsKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyRight}))
		if model.modelCursor != pageSize+2 {
			t.Fatalf("right cursor=%d want=%d", model.modelCursor, pageSize+2)
		}
		_, _ = model.handleModelsKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyRight}))
		if model.modelCursor != 2*pageSize+2 {
			t.Fatalf("second right cursor=%d want=%d", model.modelCursor, 2*pageSize+2)
		}
		_, _ = model.handleModelsKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyRight}))
		if model.modelCursor != len(model.models)-1 {
			t.Fatalf("short final page cursor=%d want=%d", model.modelCursor, len(model.models)-1)
		}
		_, _ = model.handleModelsKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyRight}))
		if model.modelCursor != len(model.models)-1 {
			t.Fatalf("right moved beyond final page: %d", model.modelCursor)
		}
		lastPage := model.modelPage(len(model.models))
		lastPageOffset := model.modelCursor - lastPage.start
		_, _ = model.handleModelsKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyLeft}))
		if model.modelCursor != lastPage.start-pageSize+lastPageOffset {
			t.Fatalf("left cursor=%d want=%d", model.modelCursor, lastPage.start-pageSize+lastPageOffset)
		}
	})

	t.Run("vertical selection crosses pages and wraps the list", func(t *testing.T) {
		model := newPaginatedModel(80, 16, 13)
		pageSize := model.modelPage(len(model.models)).size
		model.modelCursor = pageSize - 1
		_, _ = model.handleModelsKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
		if model.modelCursor != pageSize || model.modelPage(len(model.models)).number != 2 {
			t.Fatalf("down cursor=%d page=%d", model.modelCursor, model.modelPage(len(model.models)).number)
		}
		_, _ = model.handleModelsKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
		if model.modelCursor != pageSize-1 {
			t.Fatalf("up cursor=%d want=%d", model.modelCursor, pageSize-1)
		}
		model.modelCursor = len(model.models) - 1
		_, _ = model.handleModelsKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
		if model.modelCursor != 0 {
			t.Fatalf("down wrap cursor=%d", model.modelCursor)
		}
		_, _ = model.handleModelsKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
		if model.modelCursor != len(model.models)-1 {
			t.Fatalf("up wrap cursor=%d", model.modelCursor)
		}
	})

	t.Run("filtering resets to the first result", func(t *testing.T) {
		model := newPaginatedModel(80, 16, 13)
		model.models[0] = "x-first"
		model.models[8] = "x-later"
		model.modelCursor = model.modelPage(len(model.models)).size + 1
		_ = model.modelFilter.Focus()
		_, _ = model.handleModelsKey(tea.KeyPressMsg(tea.Key{Code: 'x', Text: "x"}))
		if model.modelFilter.Value() != "x" || model.modelCursor != 0 {
			t.Fatalf("filter=%q cursor=%d", model.modelFilter.Value(), model.modelCursor)
		}
	})

	t.Run("resize keeps the selected model visible", func(t *testing.T) {
		model := newPaginatedModel(80, 16, 18)
		model.modelCursor = 7
		model.resize(120, 22)
		if model.modelCursor != 7 || !strings.Contains(ansi.Strip(model.renderModels()), "> model-07") {
			t.Fatalf("expanded resize cursor=%d\n%s", model.modelCursor, ansi.Strip(model.renderModels()))
		}
		model.resize(40, 12)
		rendered := ansi.Strip(model.renderModels())
		if model.modelCursor != 7 || !strings.Contains(rendered, "> model-07") {
			t.Fatalf("compact resize cursor=%d\n%s", model.modelCursor, rendered)
		}
		compactFooter := fmt.Sprintf("%d/%d · ←/→ page · ↑/↓ select · Enter", model.modelPage(len(model.models)).number, model.modelPage(len(model.models)).total)
		if !strings.Contains(rendered, compactFooter) {
			t.Fatalf("compact footer missing %q:\n%s", compactFooter, rendered)
		}
		for _, line := range strings.Split(rendered, "\n") {
			if lipgloss.Width(line) > model.viewportContentWidth() {
				t.Fatalf("line width=%d content width=%d line=%q", lipgloss.Width(line), model.viewportContentWidth(), line)
			}
		}
	})

	t.Run("single page and refreshed results start at the first model", func(t *testing.T) {
		model := newPaginatedModel(80, 16, 3)
		rendered := ansi.Strip(model.renderModels())
		for _, modelID := range model.models {
			if !strings.Contains(rendered, modelID) {
				t.Fatalf("single page item %q missing:\n%s", modelID, rendered)
			}
		}
		if !strings.Contains(rendered, "Page 1/1") {
			t.Fatalf("single page counter missing:\n%s", rendered)
		}
		model.modelCursor = 2
		_, _ = model.Update(modelsResultMsg{models: []string{"fresh-a", "fresh-b"}})
		if model.modelCursor != 0 || len(model.models) != 2 {
			t.Fatalf("refreshed cursor=%d models=%v", model.modelCursor, model.models)
		}
	})

	t.Run("empty and manual states remain actionable", func(t *testing.T) {
		empty := newPaginatedModel(80, 16, 0)
		rendered := ansi.Strip(empty.renderModels())
		if !strings.Contains(rendered, "No models found.") || !strings.Contains(rendered, "Page 1/1") {
			t.Fatalf("empty state missing guidance:\n%s", rendered)
		}

		manual := newPaginatedModel(80, 16, 13)
		manual.modelCursor = manual.modelPage(len(manual.models)).size + 1
		manual.modelManual = true
		manual.modelFilter.Placeholder = "Manual model ID"
		manual.modelFilter.SetValue("abcd")
		manual.modelFilter.SetCursor(2)
		_ = manual.modelFilter.Focus()
		beforeCursor := manual.modelCursor
		_, _ = manual.handleModelsKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyLeft}))
		if manual.modelFilter.Position() != 1 || manual.modelCursor != beforeCursor {
			t.Fatalf("manual left input=%d model=%d", manual.modelFilter.Position(), manual.modelCursor)
		}
		_, _ = manual.handleModelsKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyRight}))
		if manual.modelFilter.Position() != 2 || manual.modelCursor != beforeCursor {
			t.Fatalf("manual right input=%d model=%d", manual.modelFilter.Position(), manual.modelCursor)
		}
		manualRendered := ansi.Strip(manual.renderModels())
		if !strings.Contains(manualRendered, "Manual ID · Enter use · Esc back") {
			t.Fatalf("manual footer missing:\n%s", manualRendered)
		}
	})
}

func TestContextWindowConfirmationAcceptsDetectedAndManualValues(t *testing.T) {
	selection := ModelSelection{Provider: "work", Model: "next-model", DetectedContextWindow: 131072, LimitSource: "models_dev"}
	t.Run("detected", func(t *testing.T) {
		backend := &fakeBackend{selection: selection}
		model := NewModel(backend, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
		model.screen = screenModels
		model.snapshot.Provider = "work"
		model.models = []string{"next-model"}
		_, selectCommand := model.handleModelsKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
		if selectCommand == nil {
			t.Fatal("model selection did not return a command")
		}
		_, _ = model.Update(selectCommand())
		view := ansi.Strip(model.View().Content)
		if model.screen != screenContextConfirm || !strings.Contains(view, "131072 tokens") || !strings.Contains(view, "Use detected value") {
			t.Fatalf("screen=%s view=%s", model.screen, view)
		}
		_, command := model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
		if command == nil {
			t.Fatal("detected confirmation did not return a command")
		}
		_, _ = model.Update(command())
		if backend.contextWindow != 131072 || model.screen != screenChat {
			t.Fatalf("context=%d screen=%s", backend.contextWindow, model.screen)
		}
	})

	t.Run("manual", func(t *testing.T) {
		backend := &fakeBackend{selection: selection}
		model := NewModel(backend, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
		_, _ = model.Update(modelSelectionMsg{selection: selection, returnTo: screenModels})
		_, _ = model.handleKey(tea.KeyPressMsg(tea.Key{Code: 'n', Text: "n"}))
		model.contextWindowConfirm.input.SetValue("200000")
		_, command := model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
		if command == nil {
			t.Fatal("manual confirmation did not return a command")
		}
		_, _ = model.Update(command())
		if backend.contextWindow != 200000 || model.screen != screenChat {
			t.Fatalf("context=%d screen=%s", backend.contextWindow, model.screen)
		}
	})
}

func TestResizeStormLongWordsAndStaleAnimationTick(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	model := NewModel(&fakeBackend{}, Options{Clock: clock, Width: 100, Height: 30})
	for index := 0; index < 100; index++ {
		width := 32 + index%90
		height := 10 + index%35
		_, _ = model.Update(tea.WindowSizeMsg{Width: width, Height: height})
	}
	if model.width < 40 || model.height < 12 || model.viewport.Width() != model.viewportContentWidth() || model.viewport.Height() != model.layout().viewportHeight {
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

func TestDurationFormattingUsesMillisecondsSecondsAndMinutes(t *testing.T) {
	tests := map[int64]string{
		0:     "0ms",
		999:   "999ms",
		1000:  "1s",
		18023: "18.023s",
		62345: "1m2.345s",
	}
	for input, expected := range tests {
		if got := FormatDurationMS(input); got != expected {
			t.Fatalf("FormatDurationMS(%d) = %q, want %q", input, got, expected)
		}
	}
}

func TestMultilineInputCompletionReferencesAndModeCycle(t *testing.T) {
	backend := &fakeBackend{files: []FileItem{{Path: "internal/ui/model.go", Size: 1234}, {Path: "space name.go", Size: 12}}}
	model := NewModel(backend, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	model.snapshot = Snapshot{Mode: "manual", Skills: []SkillItem{{Name: "review", Description: "Review repository changes carefully", Status: "active"}}}

	model.input.SetValue("/")
	_ = model.refreshCompletion()
	if labels := completionLabels(model.completion.items); !strings.Contains(labels, "/gradient") || !strings.Contains(labels, "/model") || !strings.Contains(labels, "/review") {
		t.Fatalf("slash items = %s", labels)
	}
	model.input.SetValue("/m")
	_ = model.refreshCompletion()
	labels := completionLabels(model.completion.items)
	if !strings.Contains(labels, "/model") || !strings.Contains(labels, "/mode") {
		t.Fatalf("/m items = %s", labels)
	}
	model.input.SetValue("/unknown")
	_ = model.refreshCompletion()
	if handled, _ := model.handleCompletionKey("enter"); handled {
		t.Fatal("disabled no-match completion swallowed submit")
	}

	model.files = append([]FileItem(nil), backend.files...)
	model.filesLoaded = true
	model.input.SetValue("inspect @")
	_ = model.refreshCompletion()
	labels = completionLabels(model.completion.items)
	if !strings.Contains(labels, "@skill:review") || !strings.Contains(labels, "@file:internal/ui/model.go") {
		t.Fatalf("reference items = %s", labels)
	}
	references := parseReferences("use @skill:review and @file:\"space name.go\" with\u3000@file:\"中文 路径.go\" @file:internal/ui/model.go @index.html @\"bare name.go\"")
	if len(references) != 6 || references[0] != (Reference{Kind: ReferenceSkill, Value: "review"}) || references[1].Value != "space name.go" || references[2].Value != "中文 路径.go" || references[4] != (Reference{Kind: ReferenceFile, Value: "index.html"}) || references[5].Value != "bare name.go" {
		t.Fatalf("references = %#v", references)
	}
	largeFiles := make([]FileItem, 1_000)
	for index := range largeFiles {
		largeFiles[index] = FileItem{Path: fmt.Sprintf("path/%04d.go", index)}
	}
	items, _ := referenceCompletionItems("file:0999", model.snapshot, largeFiles, true, false, "")
	if len(items) != 1 || items[0].label != "@file:path/0999.go" {
		t.Fatalf("bounded file matches = %#v", items)
	}
	unavailable, retry := referenceCompletionItems("", model.snapshot, nil, false, false, "git index failed")
	if retry || len(unavailable) == 0 || unavailable[len(unavailable)-1].label != "Files unavailable" {
		t.Fatalf("unavailable items=%#v retry=%t", unavailable, retry)
	}

	model.input.Reset()
	_, _ = model.handleChatKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter, Mod: tea.ModCtrl}))
	if model.input.Value() != "\n" {
		t.Fatalf("ctrl+enter value = %q", model.input.Value())
	}
	model.input.Reset()
	_, _ = model.handleChatKey(tea.KeyPressMsg(tea.Key{Code: 'j', Mod: tea.ModCtrl}))
	if model.input.Value() != "\n" {
		t.Fatalf("ctrl+j value = %q", model.input.Value())
	}
	model.input.Reset()
	for index := 0; index < 10; index++ {
		model.input.InsertString("line")
		_, _ = model.handleChatKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter, Mod: tea.ModShift}))
	}
	if model.input.Height() != 8 || strings.Count(model.input.Value(), "\n") != 10 {
		t.Fatalf("height=%d value=%q", model.input.Height(), model.input.Value())
	}
	model.input.Reset()
	_ = model.input.Focus()
	_, _ = model.Update(tea.PasteMsg{Content: "pasted first\npasted second"})
	if model.input.Value() != "pasted first\npasted second" || model.input.Height() != 2 {
		t.Fatalf("pasted value=%q height=%d", model.input.Value(), model.input.Height())
	}
	_, command := model.handleChatKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyTab, Mod: tea.ModShift}))
	if model.snapshot.Mode != "plan" || command == nil {
		t.Fatalf("mode=%q command=%v", model.snapshot.Mode, command)
	}
	message := command()
	_, _ = model.Update(message)
	if backend.mode != "plan" {
		t.Fatalf("backend mode = %q", backend.mode)
	}
	for _, size := range []struct{ width, height int }{{40, 12}, {80, 24}, {140, 40}} {
		model.resize(size.width, size.height)
		model.input.Reset()
		_, _ = model.Update(tea.PasteMsg{Content: strings.Repeat("line\n", 10) + "inspect @"})
		rendered := model.View()
		view := ansi.Strip(rendered.Content)
		if height := lipgloss.Height(view); height != size.height {
			t.Fatalf("%dx%d completion view height=%d\n%s", size.width, size.height, height, view)
		}
		lines := strings.Split(view, "\n")
		if rendered.Cursor == nil || rendered.Cursor.Position.Y >= len(lines) || !strings.Contains(lines[rendered.Cursor.Position.Y], "inspect @") {
			t.Fatalf("%dx%d cursor=%#v is outside final input row\n%s", size.width, size.height, rendered.Cursor, view)
		}
		for _, line := range strings.Split(view, "\n") {
			if lipgloss.Width(line) > size.width {
				t.Fatalf("%dx%d completion line width=%d line=%q", size.width, size.height, lipgloss.Width(line), line)
			}
		}
	}
}

func TestReasoningEffortCompletionSelectionAndExecution(t *testing.T) {
	backend := &fakeBackend{}
	model := NewModel(backend, Options{NoAnimation: true, NoColor: true, Width: 64, Height: 24})
	model.snapshot = Snapshot{
		Provider: "default", Model: "gpt-5.6-sol", ReasoningEffort: "high",
		SupportedReasoningEfforts: []string{"auto", "low", "medium", "high", "xhigh", "max", "ultra"},
	}

	model.input.SetValue("/effort")
	_ = model.refreshCompletion()
	if len(model.completion.items) != 7 || model.completion.items[model.completion.cursor].label != "high" || !model.completion.items[model.completion.cursor].current {
		t.Fatalf("completion=%#v cursor=%d", model.completion.items, model.completion.cursor)
	}
	if rendered := ansi.Strip(model.renderCompletion()); !strings.Contains(rendered, ">* high") {
		t.Fatalf("current effort marker missing:\n%s", rendered)
	}
	if handled, command := model.handleCompletionKey("tab"); !handled || command != nil || model.input.Value() != "/effort high" || backend.command != "" {
		t.Fatalf("tab handled=%t input=%q command=%q", handled, model.input.Value(), backend.command)
	}

	model.input.SetValue("/effort")
	_ = model.refreshCompletion()
	model.moveCompletion(1)
	if model.completion.items[model.completion.cursor].label != "xhigh" {
		t.Fatalf("cursor item=%#v", model.completion.items[model.completion.cursor])
	}
	handled, command := model.handleCompletionKey("enter")
	if !handled || command == nil || model.input.Value() != "" {
		t.Fatalf("enter handled=%t command=%v input=%q", handled, command, model.input.Value())
	}
	_ = command()
	if backend.command != "/effort xhigh" {
		t.Fatalf("command=%q", backend.command)
	}

	model.input.SetValue("/effort impossible")
	_ = model.refreshCompletion()
	if handled, _ := model.handleCompletionKey("enter"); handled {
		t.Fatal("no-match effort completion swallowed validation")
	}
	model.input.SetValue("/effort")
	_ = model.refreshCompletion()
	if handled, _ := model.handleCompletionKey("esc"); !handled || model.completion.kind != completionNone {
		t.Fatalf("escape handled=%t completion=%#v", handled, model.completion)
	}

	colored := NewModel(backend, Options{NoAnimation: true, Width: 64, Height: 24})
	colored.snapshot = model.snapshot
	colored.input.SetValue("/effort")
	_ = colored.refreshCompletion()
	coloredView := colored.renderCompletion()
	if !strings.Contains(coloredView, "\x1b[") || !strings.Contains(ansi.Strip(coloredView), ">* high") || colored.completionHeight() != 7 {
		t.Fatalf("colored completion height=%d view=%q", colored.completionHeight(), coloredView)
	}
}

func TestGradientCompletionSelectionAndExecution(t *testing.T) {
	backend := &fakeBackend{}
	model := NewModel(backend, Options{NoAnimation: true, NoColor: true, Width: 64, Height: 24})
	model.snapshot = Snapshot{GradientEnabled: false}

	model.input.SetValue("/gradient")
	_ = model.refreshCompletion()
	if len(model.completion.items) != 2 || model.completion.items[model.completion.cursor].label != "Off" || !model.completion.items[model.completion.cursor].current {
		t.Fatalf("completion=%#v cursor=%d", model.completion.items, model.completion.cursor)
	}
	if rendered := ansi.Strip(model.renderCompletion()); !strings.Contains(rendered, ">* Off") {
		t.Fatalf("current gradient marker missing:\n%s", rendered)
	}
	if handled, command := model.handleCompletionKey("tab"); !handled || command != nil || model.input.Value() != "/gradient off" {
		t.Fatalf("tab handled=%t command=%v input=%q", handled, command, model.input.Value())
	}

	model.input.SetValue("/gradient")
	_ = model.refreshCompletion()
	model.moveCompletion(-1)
	if model.completion.items[model.completion.cursor].label != "On" {
		t.Fatalf("cursor item=%#v", model.completion.items[model.completion.cursor])
	}
	handled, command := model.handleCompletionKey("enter")
	if !handled || command == nil || model.input.Value() != "" {
		t.Fatalf("enter handled=%t command=%v input=%q", handled, command, model.input.Value())
	}
	_ = command()
	if backend.command != "/gradient on" {
		t.Fatalf("command=%q", backend.command)
	}
}

func TestReasoningEffortRootCompletionTabExpandsLevels(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 64, Height: 24})
	model.snapshot = Snapshot{
		Provider: "default", Model: "gpt-5.6-sol", ReasoningEffort: "high",
		SupportedReasoningEfforts: []string{"auto", "low", "medium", "high", "xhigh", "max", "ultra"},
	}
	model.input.SetValue("/effor")
	_ = model.refreshCompletion()

	handled, command := model.handleCompletionKey("tab")
	if !handled || command != nil {
		t.Fatalf("tab handled=%t command=%v", handled, command)
	}
	if model.input.Value() != "/effort " || len(model.completion.items) != 7 {
		t.Fatalf("input=%q completion=%#v", model.input.Value(), model.completion.items)
	}
	current := model.completion.items[model.completion.cursor]
	if current.label != "high" || !current.current {
		t.Fatalf("cursor=%d current=%#v", model.completion.cursor, current)
	}
}

func TestHeaderKeepsReasoningEffortVisibleAtNarrowWidths(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 36, Height: 18})
	model.snapshot = Snapshot{Provider: "provider-with-a-long-name", Model: "model-with-a-long-name", ReasoningEffort: "ultra"}
	header := ansi.Strip(model.renderHeader())
	if !strings.HasSuffix(header, "ultra") || lipgloss.Width(header) > model.width {
		t.Fatalf("header width=%d content=%q", lipgloss.Width(header), header)
	}

	model.width = 80
	model.snapshot = Snapshot{Provider: "default", Model: "gpt-5.6-sol", ReasoningEffort: "high"}
	header = ansi.Strip(model.renderHeader())
	if !strings.Contains(header, "default  gpt-5.6-sol  high") {
		t.Fatalf("header=%q", header)
	}
}

func TestPromptHistoryNavigationPreservesDraftAndVisualBoundaries(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 40, Height: 20})
	_ = model.input.Focus()
	_, _ = model.Update(snapshotMsg{snapshot: Snapshot{SessionID: "session", PromptHistory: []string{"older\nsecond line", "newest"}}})
	model.input.SetValue("draft")
	model.input.MoveToEnd()

	_, _ = model.handleChatKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	if model.input.Value() != "newest" {
		t.Fatalf("latest history = %q", model.input.Value())
	}
	_, _ = model.handleChatKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	if model.input.Value() != "older\nsecond line" {
		t.Fatalf("older history = %q", model.input.Value())
	}
	// The recalled multiline prompt starts at its end, so Up first moves the
	// cursor through the input before it can cross the visual top boundary.
	if model.input.Line() != 1 {
		t.Fatalf("recalled cursor line = %d", model.input.Line())
	}
	_, _ = model.handleChatKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	if model.input.Value() != "newest" {
		t.Fatalf("newer history = %q", model.input.Value())
	}
	_, _ = model.handleChatKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	if model.input.Value() != "draft" {
		t.Fatalf("restored draft = %q", model.input.Value())
	}

	_, _ = model.handleChatKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	_, _ = model.handleChatKey(tea.KeyPressMsg(tea.Key{Code: 'x', Text: "x"}))
	if model.input.Value() != "newestx" || model.historyIndex != -1 {
		t.Fatalf("edited history value=%q index=%d", model.input.Value(), model.historyIndex)
	}

	model.completion = completionState{kind: completionReference, cursor: 1, items: []completionItem{{label: "one"}, {label: "two"}}}
	before := model.input.Value()
	_, _ = model.handleChatKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	if model.input.Value() != before || model.completion.cursor != 0 {
		t.Fatalf("completion priority value=%q cursor=%d", model.input.Value(), model.completion.cursor)
	}

	_, _ = model.Update(snapshotMsg{snapshot: Snapshot{SessionID: "new-session", PromptHistory: []string{}}})
	if len(model.promptHistory) != 0 || model.historyIndex != -1 {
		t.Fatalf("new session history=%#v index=%d", model.promptHistory, model.historyIndex)
	}
}

func TestSnapshotHydratesHistoryAtBottomWithoutDuplicatingLiveTimeline(t *testing.T) {
	call := protocol.ToolCall{ID: "call-read", Name: "read_file", Arguments: json.RawMessage(`{"path":"README.md"}`)}
	pendingCall := protocol.ToolCall{ID: "call-pending", Name: "bash", Arguments: json.RawMessage(`{"command":"go test ./..."}`)}
	activity := protocol.WebActivity{CallID: "web-1", Kind: protocol.ToolWebSearch, Query: "Eylu", Status: protocol.WebStatusCompleted, Sources: []protocol.WebSource{{URL: "https://example.com"}}}
	citation := protocol.URLCitation{CallID: "web-1", URL: "https://example.com", Title: "Example"}
	backend := &fakeBackend{snapshot: Snapshot{SessionID: "restored", History: []HistoryItem{
		{Kind: HistoryMessage, Role: protocol.RoleUser, Text: strings.Repeat("old question\n", 20)},
		{Kind: HistoryMessage, Role: protocol.RoleAgent, Text: "old answer"},
		{Kind: HistoryWebActivity, WebActivity: &activity},
		{Kind: HistoryCitation, Citation: &citation},
		{Kind: HistoryTool, ToolCall: &call, ToolResult: &protocol.ToolResult{CallID: "call-read", Content: "restored tool output"}},
		{Kind: HistoryTool, ToolCall: &pendingCall},
	}}}
	model := NewModel(backend, Options{NoAnimation: true, NoColor: true, Width: 50, Height: 16})
	_, _ = model.Update(snapshotMsg{snapshot: backend.snapshot})
	view := ansi.Strip(model.renderTimeline())
	if len(model.timeline) != 5 || !strings.Contains(view, "old question") || !strings.Contains(view, "old answer") || !strings.Contains(view, "Web search · completed · 1 source") || !strings.Contains(view, "Search  Eylu") || !strings.Contains(view, "> read_file  done") || !strings.Contains(view, "> bash  interrupted") {
		t.Fatalf("timeline=%#v\n%s", model.timeline, view)
	}
	if model.timeline[3].tool.arguments != string(call.Arguments) || model.timeline[3].tool.content != "restored tool output" {
		t.Fatalf("restored tool detail=%#v", model.timeline[3].tool)
	}
	if !model.viewport.AtBottom() || !model.followOutput {
		t.Fatalf("viewport bottom=%t follow=%t offset=%d", model.viewport.AtBottom(), model.followOutput, model.viewport.YOffset())
	}
	model.appendNotice("live notice", false)
	_, _ = model.Update(snapshotMsg{snapshot: backend.snapshot})
	if len(model.timeline) != 6 || model.timeline[5].text != "live notice" {
		t.Fatalf("same-session snapshot replaced live timeline: %#v", model.timeline)
	}
}

func TestSessionChangeReplacesHistoryBeforeAppendingCommandNotice(t *testing.T) {
	backend := &fakeBackend{snapshot: Snapshot{SessionID: "old", History: []HistoryItem{{Kind: HistoryMessage, Role: protocol.RoleUser, Text: "old history"}}}}
	model := NewModel(backend, Options{NoAnimation: true, NoColor: true, Width: 60, Height: 16})
	_, _ = model.Update(snapshotMsg{snapshot: backend.snapshot})
	backend.snapshot = Snapshot{SessionID: "new"}
	_, command := model.Update(commandResultMsg{text: "Closed session old. New session new."})
	if command == nil {
		t.Fatal("command result did not request a refreshed snapshot")
	}
	_, _ = model.Update(command())
	view := ansi.Strip(model.renderTimeline())
	if strings.Contains(view, "old history") || !strings.Contains(view, "Closed session old. New session new.") || len(model.timeline) != 1 {
		t.Fatalf("timeline=%#v\n%s", model.timeline, view)
	}
}

func TestViewRequestsKeyboardEnhancements(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true})
	view := model.View()
	if !view.KeyboardEnhancements.ReportAllKeysAsEscapeCodes || !view.KeyboardEnhancements.ReportAssociatedText {
		t.Fatalf("keyboard enhancements = %#v", view.KeyboardEnhancements)
	}
}

func TestMouseSelectionCopiesPlainWideTextAndShowsToast(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)}
	copied := ""
	model := NewModel(&fakeBackend{}, Options{
		NoAnimation: true, Width: 40, Height: 16, Clock: clock,
		ClipboardWrite: func(value string) error { copied = value; return nil },
	})
	model.viewport.SetContent("alpha\n你好 world")
	model.viewport.SetHeight(4)
	_, _ = model.handleMouse(tea.MouseClickMsg{X: 3, Y: 1, Button: tea.MouseLeft})
	_, _ = model.handleMouse(tea.MouseMotionMsg{X: 6, Y: 2, Button: tea.MouseLeft})
	_, command := model.handleMouse(tea.MouseReleaseMsg{X: 6, Y: 2, Button: tea.MouseLeft})
	if command == nil {
		t.Fatal("selection did not produce clipboard command")
	}
	message := command()
	_, toastCommand := model.Update(message)
	if copied != "lpha\n你好 " || model.copyToast != "8 chars copied" || toastCommand == nil {
		t.Fatalf("copied=%q toast=%q command=%v", copied, model.copyToast, toastCommand)
	}
	if !strings.Contains(model.View().Content, "8 chars copied") {
		t.Fatalf("toast missing from view: %q", model.View().Content)
	}
	_, _ = model.Update(toastCommand())
	if model.copyToast != "" {
		t.Fatalf("toast did not expire: %q", model.copyToast)
	}
}

func TestMouseSelectionPreservesHardBreaksAndRemovesSoftWrapAndOSC(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 40, Height: 16})
	model.viewport.SetWidth(5)
	model.viewport.SetHeight(2)
	model.viewport.SetContent("zero\n\x1b]8;;https://example.com\x07abcdefghij\x1b]8;;\x07\nlast")
	model.viewport.SetYOffset(1)
	lines := visibleSelectionLines(model.viewport.GetContent(), model.viewport.Width(), model.viewport.Height(), model.viewport.YOffset())
	selected := selectedText(lines, selectionPoint{row: 0, col: 0}, selectionPoint{row: 1, col: 4})
	if selected != "abcdefghij" || strings.Contains(selected, "\x1b") {
		t.Fatalf("selected = %q", selected)
	}

	hardLines := visibleSelectionLines("first\n第二", 40, 2, 0)
	hardSelected := selectedText(hardLines, selectionPoint{row: 0, col: 0}, selectionPoint{row: 1, col: 3})
	if hardSelected != "first\n第二" {
		t.Fatalf("hard selection = %q", hardSelected)
	}
	wideLines := selectionLines("你好", 40)
	if wideSelected := selectedText(wideLines, selectionPoint{row: 0, col: 1}, selectionPoint{row: 0, col: 1}); wideSelected != "你" {
		t.Fatalf("wide-cell selection = %q", wideSelected)
	}

	styled := NewModel(&fakeBackend{}, Options{NoAnimation: true, Width: 40, Height: 16})
	styled.viewport.SetHeight(1)
	styled.viewport.SetContent("\x1b[31mred\x1b[0m plain")
	styled.selection = selectionState{
		active: true,
		anchor: selectionPoint{row: 0, col: 0},
		focus:  selectionPoint{row: 0, col: 8},
		lines:  selectionLines(styled.viewport.GetContent(), styled.viewport.Width()),
	}
	if rendered := styled.renderViewport(); !strings.Contains(rendered, styled.styles.Selection.Render("red plain")) {
		t.Fatalf("selection background was interrupted by nested ANSI: %q", rendered)
	}
}

func TestMouseSelectionScrollsBeyondViewportAndCopiesFullHistory(t *testing.T) {
	copied := ""
	model := NewModel(&fakeBackend{}, Options{
		NoAnimation: true, NoColor: true, Width: 40, Height: 16,
		ClipboardWrite: func(value string) error { copied = value; return nil },
	})
	model.viewport.SetWidth(40)
	model.viewport.SetHeight(3)
	model.viewport.SetContent("line-00\nline-01\nline-02\nline-03\nline-04\nline-05\nline-06")
	_, _ = model.handleMouse(tea.MouseClickMsg{X: 2, Y: 1, Button: tea.MouseLeft})
	_, _ = model.handleMouse(tea.MouseMotionMsg{X: 8, Y: 3, Button: tea.MouseLeft})
	_, _ = model.handleMouse(tea.MouseWheelMsg{X: 8, Y: 3, Button: tea.MouseWheelDown})
	_, command := model.handleMouse(tea.MouseReleaseMsg{X: 8, Y: 3, Button: tea.MouseLeft})
	if command == nil {
		t.Fatal("scrolling selection did not produce clipboard command")
	}
	_ = command()
	if copied != "line-00\nline-01\nline-02\nline-03\nline-04\nline-05" {
		t.Fatalf("copied across viewport = %q", copied)
	}
}

func TestClipboardFailureShowsEnglishToast(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{
		NoAnimation: true, NoColor: true,
		ClipboardWrite: func(string) error { return errors.New("clipboard unavailable") },
	})
	model.copyToastSequence = 1
	command := model.handleClipboardResult(clipboardResultMsg{sequence: 1, err: errors.New("clipboard unavailable")})
	if model.copyToast != "Copy failed" || command == nil {
		t.Fatalf("toast=%q command=%v", model.copyToast, command)
	}
}

func TestModeCycleQueuesDuringActiveRequestAndSkillAliasExecutes(t *testing.T) {
	backend := &fakeBackend{}
	model := NewModel(backend, Options{NoAnimation: true, NoColor: true})
	model.snapshot = Snapshot{Mode: "manual", Skills: []SkillItem{{Name: "review", Status: "active"}}}
	model.state = StateStreaming
	_, command := model.handleChatKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyTab, Mod: tea.ModShift}))
	if command != nil || model.queuedMode != "plan" || model.snapshot.Mode != "manual" {
		t.Fatalf("command=%v queued=%q mode=%q", command, model.queuedMode, model.snapshot.Mode)
	}
	queued := model.queuedMode
	model.state = StateCompleted
	model.operationID = "op-mode"
	_, command = model.Update(backendEventClosedMsg{operationID: "op-mode"})
	if command == nil {
		t.Fatal("queued mode did not produce a command")
	}
	// Batch commands execute independently; exercising SetMode directly verifies
	// the queued target while the model-level test covers queue ownership.
	if err := backend.SetMode(context.Background(), queued); err != nil {
		t.Fatal(err)
	}
	if backend.mode != "plan" {
		t.Fatalf("backend mode = %q", backend.mode)
	}

	model.state = StateIdle
	_, aliasCommand := model.executeSlash("/review")
	if aliasCommand == nil {
		t.Fatal("top-level skill alias did not map to a backend command")
	}

	backend.err = errors.New("persist mode failed")
	model.snapshot.Mode = "manual"
	_, rollbackCommand := model.requestMode("plan")
	if rollbackCommand == nil || model.snapshot.Mode != "plan" {
		t.Fatalf("optimistic mode=%q command=%v", model.snapshot.Mode, rollbackCommand)
	}
	_, _ = model.Update(rollbackCommand())
	if model.snapshot.Mode != "manual" {
		t.Fatalf("mode after persistence failure = %q", model.snapshot.Mode)
	}

	model.operationID = "op-reference-error"
	model.state = StateFailed
	model.draft = `inspect @file:"missing.go"`
	model.input.Reset()
	_, _ = model.Update(backendEventClosedMsg{operationID: model.operationID})
	if model.input.Value() != model.draft {
		t.Fatalf("restored draft = %q", model.input.Value())
	}
}

func completionLabels(items []completionItem) string {
	labels := make([]string, len(items))
	for index, item := range items {
		labels[index] = item.label
	}
	return strings.Join(labels, " ")
}
