package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"Eylu/internal/protocol"
)

type screenKind string

const (
	screenChat         screenKind = "chat"
	screenProviders    screenKind = "providers"
	screenProviderForm screenKind = "provider_form"
	screenModels       screenKind = "models"
	screenSkills       screenKind = "skills"
	screenContext      screenKind = "context"
	screenToolDetail   screenKind = "tool_detail"
)

type timelineKind string

const (
	timelineMessage timelineKind = "message"
	timelineTool    timelineKind = "tool"
	timelineNotice  timelineKind = "notice"
)

type timelineItem struct {
	kind              timelineKind
	role              string
	text              string
	tool              *toolView
	err               bool
	renderedSource    string
	renderedText      string
	renderedWidth     int
	renderedWorkspace string
	renderedNoColor   bool
}

type toolView struct {
	name                 string
	callID               string
	outputIndex          int
	argumentBuffer       strings.Builder
	arguments            string
	previewArgumentBytes int
	content              string
	path                 string
	preview              string
	generatedBytes       int
	generatedLines       int
	preparing            bool
	running              bool
	isError              bool
	truncated            bool
	durationMS           int64
	exitCode             int
}

type Model struct {
	backend Backend
	context context.Context
	clock   Clock
	styles  Styles

	input       textarea.Model
	viewport    viewport.Model
	spinner     spinner.Model
	modelFilter textinput.Model
	form        providerFormModel

	width  int
	height int
	screen screenKind
	state  OperationState

	snapshot       Snapshot
	timeline       []timelineItem
	providerCursor int
	skillCursor    int
	modelCursor    int
	toolCursor     int
	models         []string
	modelManual    bool
	contextExpand  bool
	approval       *ApprovalRequest

	operationID          string
	eventChannel         chan Event
	cancel               context.CancelFunc
	startedAt            time.Time
	retryAt              time.Time
	cancelRequested      bool
	activity             Activity
	operationUsage       protocol.Usage
	streamedBytes        int
	reasoningBytes       int
	roundReasoningTokens int
	roundReasoningExact  bool
	followOutput         bool
	animation            bool
	noColor              bool
	operationSeq         uint64
	markdown             markdownRenderCache
}

type snapshotMsg struct {
	snapshot Snapshot
	err      error
}

type backendEventMsg struct {
	event Event
}

type backendEventClosedMsg struct{ operationID string }
type backendWorkerMsg struct{ operationID string }

type commandResultMsg struct {
	text string
	err  error
}

type mutationResultMsg struct{ err error }

type modelsResultMsg struct {
	models []string
	err    error
}

type operationSpinnerMsg struct {
	operationID string
	message     tea.Msg
}

type transitionMsg struct {
	operationID string
	state       OperationState
}

type interruptRequestMsg struct{}

func NewModel(backend Backend, options Options) *Model {
	clock := options.Clock
	if clock == nil {
		clock = realClock{}
	}
	width, height := options.Width, options.Height
	if width <= 0 {
		width = 100
	}
	if height <= 0 {
		height = 30
	}
	baseContext := options.Context
	if baseContext == nil {
		baseContext = context.Background()
	}
	input := textarea.New()
	input.Placeholder = "Message Eylu"
	input.Prompt = "> "
	input.ShowLineNumbers = false
	input.DynamicHeight = false
	input.SetHeight(1)
	input.CharLimit = 1 << 20
	input.SetVirtualCursor(false)
	inputStyles := input.Styles()
	inputStyles.Cursor.Shape = tea.CursorBar
	inputStyles.Cursor.Blink = true
	input.SetStyles(inputStyles)
	input.SetWidth(width)
	filter := textinput.New()
	filter.Placeholder = "Filter models"
	filter.SetVirtualCursor(true)
	filter.SetWidth(max(20, width-6))
	model := &Model{
		backend: backend, context: baseContext, clock: clock, styles: DefaultStyles(options.NoColor), input: input,
		viewport: viewport.New(viewport.WithWidth(width), viewport.WithHeight(max(5, height-8))),
		spinner:  spinner.New(spinner.WithSpinner(spinner.Dot)), modelFilter: filter,
		width: width, height: height, screen: screenChat, state: StateIdle, followOutput: true,
		animation: !options.NoAnimation, noColor: options.NoColor,
	}
	model.viewport.SoftWrap = true
	model.resize(width, height)
	return model
}

func Run(backend Backend, options Options) error {
	programOptions := make([]tea.ProgramOption, 0, 4)
	if options.Input != nil {
		programOptions = append(programOptions, tea.WithInput(options.Input))
	}
	if options.Output != nil {
		programOptions = append(programOptions, tea.WithOutput(options.Output))
	}
	if options.Context != nil {
		programOptions = append(programOptions, tea.WithContext(options.Context))
	}
	programOptions = append(programOptions, tea.WithoutSignalHandler())
	program := tea.NewProgram(NewModel(backend, options), programOptions...)
	interrupts := make(chan os.Signal, 2)
	stopped := make(chan struct{})
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	defer close(stopped)
	go func() {
		for {
			select {
			case <-stopped:
				return
			case <-interrupts:
				program.Send(interruptRequestMsg{})
			}
		}
	}()
	_, err := program.Run()
	return err
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.input.Focus(), m.loadSnapshotCmd())
}

func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch typed := message.(type) {
	case tea.WindowSizeMsg:
		m.resize(typed.Width, typed.Height)
		return m, nil
	case snapshotMsg:
		if typed.err != nil {
			m.appendNotice(typed.err.Error(), true)
		} else {
			m.snapshot = typed.snapshot
		}
		m.refreshViewport()
		return m, nil
	case backendEventMsg:
		return m.handleBackendEvent(typed.event)
	case backendEventClosedMsg:
		if typed.operationID == m.operationID {
			if m.state == StateCancelling {
				m.state = StateCancelled
			} else if m.state != StateFailed && m.state != StateCancelled {
				m.state = StateCompleted
			}
			m.cancel = nil
			m.cancelRequested = false
			m.eventChannel = nil
		}
		return m, m.loadSnapshotCmd()
	case backendWorkerMsg:
		return m, nil
	case commandResultMsg:
		if typed.err != nil {
			m.appendNotice(typed.err.Error(), true)
		} else if typed.text != "" {
			m.appendNotice(typed.text, false)
		}
		m.screen = screenChat
		m.refreshViewport()
		return m, m.loadSnapshotCmd()
	case mutationResultMsg:
		if typed.err != nil {
			m.appendNotice(typed.err.Error(), true)
		} else {
			m.appendNotice("Provider configuration updated.", false)
		}
		m.screen = screenProviders
		m.form = providerFormModel{}
		return m, m.loadSnapshotCmd()
	case modelsResultMsg:
		if typed.err != nil {
			m.state = StateFailed
			m.appendNotice(typed.err.Error(), true)
		} else {
			m.models = append(m.models[:0], typed.models...)
			m.modelCursor = 0
			m.state = StateIdle
		}
		return m, nil
	case operationSpinnerMsg:
		if typed.operationID != m.operationID || !m.busy() || !m.animation {
			return m, nil
		}
		updated, command := m.spinner.Update(typed.message)
		m.spinner = updated
		return m, wrapOperationCmd(m.operationID, command)
	case transitionMsg:
		if typed.operationID == m.operationID && m.state != StateFailed && m.state != StateCancelling {
			m.state = typed.state
		}
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(typed)
	case interruptRequestMsg:
		return m.handleInterrupt()
	case tea.PasteMsg:
		if m.screen == screenChat {
			updated, command := m.input.Update(message)
			m.input = updated
			return m, command
		}
	}
	if m.screen == screenChat {
		updated, command := m.input.Update(message)
		m.input = updated
		return m, command
	}
	return m, nil
}

func (m *Model) handleBackendEvent(event Event) (tea.Model, tea.Cmd) {
	if event.OperationID != "" && event.OperationID != m.operationID {
		return m, nil
	}
	var command tea.Cmd
	switch event.Kind {
	case EventState:
		m.state = event.State
		if event.State == StateRetryBackoff && event.RetryAfter > 0 {
			m.retryAt = m.clock.Now().Add(event.RetryAfter)
		}
	case EventActivity:
		if event.Activity != nil {
			if event.Activity.ReasoningKnown {
				m.activity.Reasoning = event.Activity.Reasoning
				m.activity.ReasoningKnown = true
			}
			if event.Activity.TokenBytesPerToken > 0 {
				m.activity.TokenBytesPerToken = event.Activity.TokenBytesPerToken
			}
			if event.Activity.InputTokens > 0 {
				m.activity.InputTokens = event.Activity.InputTokens
				m.activity.InputExact = event.Activity.InputExact
				m.reasoningBytes = 0
				m.roundReasoningTokens = 0
				m.roundReasoningExact = false
			}
		}
	case EventReasoningDelta:
		m.activity.Reasoning = true
		m.activity.ReasoningKnown = true
		m.reasoningBytes += len([]byte(event.Delta))
	case EventTextDelta:
		m.state = StateStreaming
		m.streamedBytes += len([]byte(event.Delta))
		m.appendAgentDelta(event.Delta)
	case EventToolCallDelta:
		m.state = StatePreparingTool
		if event.ToolCallDelta != nil {
			m.streamedBytes += len([]byte(event.ToolCallDelta.Delta))
			m.applyToolCallDelta(*event.ToolCallDelta)
		}
	case EventToolStart:
		m.state = StateExecutingTool
		if event.ToolCall != nil {
			m.startTool(*event.ToolCall)
		}
	case EventToolResult:
		if event.ToolResult != nil {
			m.completeTool(event.ToolResult)
			m.state = StateWaitingFirstToken
			command = m.transitionAfter(m.operationID, StateWaitingFirstToken)
		}
	case EventToolAudit:
		if event.ToolAudit != nil {
			m.applyToolAudit(*event.ToolAudit)
		}
	case EventApproval:
		m.approval = event.Approval
		m.state = StateAwaitingApproval
	case EventContext:
		if event.Context != nil {
			m.snapshot.Context = *event.Context
		}
	case EventUsage:
		if event.Usage != nil && event.Usage.Exact {
			m.operationUsage.InputTokens += event.Usage.InputTokens
			m.operationUsage.OutputTokens += event.Usage.OutputTokens
			m.operationUsage.ReasoningTokens += event.Usage.ReasoningTokens
			m.operationUsage.Exact = true
			m.activity.InputTokens = event.Usage.InputTokens
			m.activity.InputExact = true
			m.streamedBytes = 0
			m.reasoningBytes = 0
			m.roundReasoningTokens = event.Usage.ReasoningTokens
			m.roundReasoningExact = true
		}
	case EventNotice:
		m.appendNotice(event.Notice, event.Error)
	}
	m.refreshViewport()
	if m.eventChannel != nil && event.Kind != EventApproval {
		command = tea.Batch(command, waitEventCmd(m.operationID, m.eventChannel))
	}
	return m, command
}

func (m *Model) handleKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	if key == "ctrl+c" {
		return m.handleInterrupt()
	}
	if m.approval != nil {
		return m.handleApprovalKey(key)
	}
	switch m.screen {
	case screenProviders:
		return m.handleProvidersKey(key)
	case screenProviderForm:
		return m.handleProviderFormKey(message)
	case screenModels:
		return m.handleModelsKey(message)
	case screenSkills:
		return m.handleSkillsKey(key)
	case screenContext:
		if key == "esc" {
			m.screen = screenChat
		} else if key == "enter" {
			m.contextExpand = !m.contextExpand
		}
		return m, nil
	case screenToolDetail:
		if key == "esc" {
			m.screen = screenChat
			m.refreshViewport()
			return m, nil
		}
		updated, command := m.viewport.Update(message)
		m.viewport = updated
		return m, command
	}
	return m.handleChatKey(message)
}

func (m *Model) handleInterrupt() (tea.Model, tea.Cmd) {
	if m.busy() && !m.cancelRequested {
		m.cancelRequested = true
		m.state = StateCancelling
		if m.cancel != nil {
			m.cancel()
		}
		m.appendNotice("Cancellation requested.", false)
		m.refreshViewport()
		return m, nil
	}
	return m, tea.Quit
}

func (m *Model) handleChatKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	switch key {
	case "enter":
		value := strings.TrimSpace(m.input.Value())
		if value == "" {
			return m, nil
		}
		if strings.HasPrefix(value, "/") {
			m.input.Reset()
			return m.executeSlash(value)
		}
		if m.busy() {
			m.appendNotice("A request is already running.", true)
			m.refreshViewport()
			return m, nil
		}
		m.input.Reset()
		return m.startRequest(value)
	case "tab":
		if completed := m.completeSkillInput(); completed {
			return m, nil
		}
	case "pgup":
		m.followOutput = false
		m.viewport.PageUp()
		return m, nil
	case "pgdown":
		m.viewport.PageDown()
		m.followOutput = m.viewport.AtBottom()
		return m, nil
	case "ctrl+t":
		if index := m.lastToolIndex(); index >= 0 {
			m.toolCursor = index
			m.screen = screenToolDetail
			m.refreshViewport()
		}
		return m, nil
	}
	updated, command := m.input.Update(message)
	m.input = updated
	return m, command
}

func (m *Model) startRequest(prompt string) (tea.Model, tea.Cmd) {
	if m.busy() {
		m.appendNotice("A request is already running.", true)
		m.refreshViewport()
		return m, nil
	}
	sequence := atomic.AddUint64(&m.operationSeq, 1)
	m.operationID = fmt.Sprintf("op-%d", sequence)
	m.eventChannel = make(chan Event, 256)
	requestContext, cancel := context.WithCancel(m.context)
	m.cancel = cancel
	m.cancelRequested = false
	m.startedAt = m.clock.Now()
	m.activity = Activity{TokenBytesPerToken: 4}
	m.operationUsage = protocol.Usage{}
	m.streamedBytes = 0
	m.reasoningBytes = 0
	m.roundReasoningTokens = 0
	m.roundReasoningExact = false
	m.state = StateConnecting
	m.timeline = append(m.timeline, timelineItem{kind: timelineMessage, role: "user", text: prompt})
	m.followOutput = true
	m.refreshViewport()
	commands := []tea.Cmd{runBackendCmd(requestContext, m.backend, m.operationID, prompt, m.eventChannel), waitEventCmd(m.operationID, m.eventChannel)}
	if m.animation {
		commands = append(commands, wrapOperationCmd(m.operationID, func() tea.Msg { return m.spinner.Tick() }))
	}
	return m, tea.Batch(commands...)
}

func (m *Model) executeSlash(line string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(line)
	command := fields[0]
	switch command {
	case "/quit":
		return m, tea.Quit
	case "/context":
		m.screen = screenContext
		return m, m.loadSnapshotCmd()
	case "/skills":
		m.screen = screenSkills
		return m, m.loadSnapshotCmd()
	case "/providers":
		m.screen = screenProviders
		return m, m.loadSnapshotCmd()
	case "/provider":
		if len(fields) >= 2 && fields[1] == "add" {
			m.form = newProviderFormModel(ProviderForm{Adapter: "openai_responses"}, m.width)
			m.screen = screenProviderForm
			return m, nil
		}
		if len(fields) == 3 && fields[1] == "edit" {
			if item, ok := m.providerByName(fields[2]); ok {
				m.openProviderForm(item)
				return m, nil
			}
		}
	case "/model":
		if len(fields) == 1 {
			m.screen = screenModels
			return m, tea.Batch(m.modelFilter.Focus(), m.fetchModelsCmd())
		}
	case "/help":
		m.appendNotice("/new /context /skills /skill /providers /provider /model /mode /quit", false)
		m.refreshViewport()
		return m, nil
	}
	return m, m.commandCmd(line)
}

func (m *Model) handleApprovalKey(key string) (tea.Model, tea.Cmd) {
	approved := false
	switch strings.ToLower(key) {
	case "y", "enter":
		approved = true
	case "n", "esc":
	default:
		return m, nil
	}
	select {
	case m.approval.Response <- approved:
	default:
	}
	m.approval = nil
	m.state = StateExecutingTool
	return m, waitEventCmd(m.operationID, m.eventChannel)
}

func (m *Model) busy() bool {
	switch m.state {
	case StateConnecting, StateFetchingModels, StateWaitingFirstToken, StateStreaming, StatePreparingTool, StateExecutingTool, StateAwaitingApproval, StateRetryBackoff, StateCancelling:
		return true
	default:
		return false
	}
}

func runBackendCmd(ctx context.Context, backend Backend, operationID, prompt string, events chan Event) tea.Cmd {
	return func() tea.Msg {
		enqueue := func(event Event, lossy bool) {
			if lossy {
				select {
				case events <- event:
				default:
				}
				return
			}
			select {
			case events <- event:
			case <-ctx.Done():
				select {
				case events <- event:
				default:
				}
			}
		}
		err := backend.Submit(ctx, operationID, prompt, func(event Event) {
			if event.OperationID == "" {
				event.OperationID = operationID
			}
			enqueue(event, event.Kind == EventToolCallDelta)
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				enqueue(Event{OperationID: operationID, Kind: EventNotice, Notice: "Request cancelled."}, false)
				enqueue(Event{OperationID: operationID, Kind: EventState, State: StateCancelled}, false)
			} else {
				enqueue(Event{OperationID: operationID, Kind: EventNotice, Notice: err.Error(), Error: true}, false)
				enqueue(Event{OperationID: operationID, Kind: EventState, State: StateFailed}, false)
			}
		} else {
			enqueue(Event{OperationID: operationID, Kind: EventState, State: StateCompleted}, false)
		}
		close(events)
		return backendWorkerMsg{operationID: operationID}
	}
}

func waitEventCmd(operationID string, events <-chan Event) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-events
		if !ok {
			return backendEventClosedMsg{operationID: operationID}
		}
		return backendEventMsg{event: event}
	}
}

func wrapOperationCmd(operationID string, command tea.Cmd) tea.Cmd {
	if command == nil {
		return nil
	}
	return func() tea.Msg { return operationSpinnerMsg{operationID: operationID, message: command()} }
}

func (m *Model) transitionAfter(operationID string, state OperationState) tea.Cmd {
	return m.clock.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return transitionMsg{operationID: operationID, state: state} })
}

func (m *Model) loadSnapshotCmd() tea.Cmd {
	return func() tea.Msg {
		snapshot, err := m.backend.Snapshot(m.context)
		return snapshotMsg{snapshot: snapshot, err: err}
	}
}

func (m *Model) commandCmd(line string) tea.Cmd {
	return func() tea.Msg {
		text, err := m.backend.Command(m.context, line)
		return commandResultMsg{text: text, err: err}
	}
}

func (m *Model) appendAgentDelta(delta string) {
	if delta == "" {
		return
	}
	if len(m.timeline) == 0 || m.timeline[len(m.timeline)-1].kind != timelineMessage || m.timeline[len(m.timeline)-1].role != "agent" {
		m.timeline = append(m.timeline, timelineItem{kind: timelineMessage, role: "agent"})
	}
	m.timeline[len(m.timeline)-1].text += delta
}

func (m *Model) appendNotice(text string, isError bool) {
	if strings.TrimSpace(text) != "" {
		m.timeline = append(m.timeline, timelineItem{kind: timelineNotice, text: text, err: isError})
	}
}

func (m *Model) completeTool(result *protocol.ToolResult) {
	for index := len(m.timeline) - 1; index >= 0; index-- {
		item := &m.timeline[index]
		if item.kind == timelineTool && item.tool != nil && item.tool.callID == result.CallID {
			item.tool.preparing = false
			item.tool.running = false
			item.tool.content = result.Content
			item.tool.isError = result.IsError
			item.tool.truncated = result.Truncated
			if path, ok := result.Metadata["path"].(string); ok && item.tool.path == "" {
				item.tool.path = path
			}
			if bytes, ok := result.Metadata["bytes"].(int); ok {
				item.tool.generatedBytes = bytes
			}
			if lines, ok := result.Metadata["lines"].(int); ok {
				item.tool.generatedLines = lines
			}
			return
		}
	}
}

func (m *Model) applyToolCallDelta(update protocol.ToolCallDelta) {
	view := m.findTool(update.ID, update.OutputIndex)
	if view == nil {
		view = &toolView{callID: update.ID, outputIndex: update.OutputIndex}
		m.timeline = append(m.timeline, timelineItem{kind: timelineTool, tool: view})
	}
	if update.ID != "" {
		view.callID = update.ID
	}
	if update.Name != "" {
		view.name = update.Name
	}
	if update.Arguments != "" {
		view.replaceArguments(update.Arguments)
	} else if update.Delta != "" {
		view.appendArguments(update.Delta)
	}
	view.preparing = true
	view.running = false
	if view.path == "" || view.preview == "" || update.Done || len(view.arguments)-view.previewArgumentBytes >= 256 || strings.Contains(update.Delta, `\n`) {
		updateFileToolPreview(view)
	}
}

func (m *Model) startTool(call protocol.ToolCall) {
	view := m.findTool(call.ID, -1)
	if view == nil {
		view = &toolView{callID: call.ID}
		m.timeline = append(m.timeline, timelineItem{kind: timelineTool, tool: view})
	}
	view.name = call.Name
	view.replaceArguments(string(call.Arguments))
	view.preparing = false
	view.running = true
	updateFileToolPreview(view)
}

func (view *toolView) replaceArguments(arguments string) {
	view.argumentBuffer.Reset()
	view.argumentBuffer.WriteString(arguments)
	view.arguments = view.argumentBuffer.String()
}

func (view *toolView) appendArguments(delta string) {
	if view.argumentBuffer.Len() == 0 && view.arguments != "" {
		view.argumentBuffer.WriteString(view.arguments)
	}
	view.argumentBuffer.WriteString(delta)
	view.arguments = view.argumentBuffer.String()
}

func (m *Model) findTool(callID string, outputIndex int) *toolView {
	for index := len(m.timeline) - 1; index >= 0; index-- {
		item := m.timeline[index]
		if item.kind != timelineTool || item.tool == nil {
			continue
		}
		if callID != "" && item.tool.callID == callID {
			return item.tool
		}
		if callID == "" && outputIndex >= 0 && item.tool.preparing && item.tool.outputIndex == outputIndex {
			return item.tool
		}
	}
	return nil
}

func updateFileToolPreview(view *toolView) {
	if view == nil {
		return
	}
	path, _ := partialJSONStringField(view.arguments, "path")
	view.path = path
	view.previewArgumentBytes = len(view.arguments)
	switch view.name {
	case "write_file":
		content, started := partialJSONStringField(view.arguments, "content")
		if !started {
			return
		}
		view.generatedBytes = len([]byte(content))
		view.generatedLines = countTextLines(content)
		view.preview = prefixedTail(content, "+ ", 6)
	case "edit_file":
		oldString, oldStarted := partialJSONStringField(view.arguments, "old_string")
		newString, newStarted := partialJSONStringField(view.arguments, "new_string")
		view.generatedBytes = len([]byte(oldString)) + len([]byte(newString))
		view.generatedLines = countTextLines(newString)
		parts := make([]string, 0, 2)
		if oldStarted {
			parts = append(parts, prefixedTail(oldString, "- ", 2))
		}
		if newStarted {
			parts = append(parts, prefixedTail(newString, "+ ", 4))
		}
		view.preview = strings.Join(parts, "\n")
	}
}

func partialJSONStringField(arguments, field string) (string, bool) {
	marker := `"` + field + `"`
	index := strings.Index(arguments, marker)
	if index < 0 {
		return "", false
	}
	rest := arguments[index+len(marker):]
	rest = strings.TrimLeft(rest, " \t\r\n")
	if len(rest) == 0 || rest[0] != ':' {
		return "", false
	}
	rest = strings.TrimLeft(rest[1:], " \t\r\n")
	if len(rest) == 0 || rest[0] != '"' {
		return "", false
	}
	encoded := rest[1:]
	escaped := false
	for offset := 0; offset < len(encoded); offset++ {
		switch {
		case escaped:
			escaped = false
		case encoded[offset] == '\\':
			escaped = true
		case encoded[offset] == '"':
			encoded = encoded[:offset]
			offset = len(encoded)
		}
	}
	for trim := 0; trim <= 12 && trim <= len(encoded); trim++ {
		candidate := `"` + encoded[:len(encoded)-trim] + `"`
		var value string
		if json.Unmarshal([]byte(candidate), &value) == nil {
			return value, true
		}
	}
	return "", true
}

func prefixedTail(value, prefix string, limit int) string {
	if value == "" || limit <= 0 {
		return ""
	}
	lines := strings.Split(value, "\n")
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	for index := range lines {
		runes := []rune(lines[index])
		if len(runes) > 240 {
			lines[index] = "..." + string(runes[len(runes)-237:])
		}
		lines[index] = prefix + lines[index]
	}
	return strings.Join(lines, "\n")
}

func countTextLines(value string) int {
	if value == "" {
		return 0
	}
	return strings.Count(value, "\n") + 1
}

func (m *Model) applyToolAudit(audit ToolAudit) {
	for index := range m.timeline {
		item := &m.timeline[index]
		if item.kind == timelineTool && item.tool != nil && item.tool.callID == audit.CallID {
			item.tool.durationMS = audit.DurationMS
			item.tool.exitCode = audit.ExitCode
		}
	}
}

func (m *Model) completeSkillInput() bool {
	value := m.input.Value()
	if !strings.HasPrefix(value, "/skill ") {
		return false
	}
	prefix := strings.TrimSpace(strings.TrimPrefix(value, "/skill "))
	for _, item := range m.snapshot.Skills {
		if strings.HasPrefix(item.Name, prefix) {
			m.input.SetValue("/skill " + item.Name)
			m.input.MoveToEnd()
			return true
		}
	}
	return false
}

func (m *Model) lastToolIndex() int {
	for index := len(m.timeline) - 1; index >= 0; index-- {
		if m.timeline[index].kind == timelineTool {
			return index
		}
	}
	return -1
}
