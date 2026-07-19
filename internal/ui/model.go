package ui

import (
	"context"
	"fmt"
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
	kind timelineKind
	role string
	text string
	tool *toolView
	err  bool
}

type toolView struct {
	name       string
	callID     string
	arguments  string
	content    string
	running    bool
	isError    bool
	truncated  bool
	durationMS int64
	exitCode   int
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

	operationID     string
	eventChannel    chan Event
	cancel          context.CancelFunc
	startedAt       time.Time
	retryAt         time.Time
	cancelRequested bool
	followOutput    bool
	animation       bool
	noColor         bool
	operationSeq    uint64
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
	input.SetHeight(3)
	input.CharLimit = 1 << 20
	input.SetVirtualCursor(true)
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
	programOptions := make([]tea.ProgramOption, 0, 2)
	if options.Input != nil {
		programOptions = append(programOptions, tea.WithInput(options.Input))
	}
	if options.Output != nil {
		programOptions = append(programOptions, tea.WithOutput(options.Output))
	}
	if options.Context != nil {
		programOptions = append(programOptions, tea.WithContext(options.Context))
	}
	_, err := tea.NewProgram(NewModel(backend, options), programOptions...).Run()
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
			if m.state != StateFailed && m.state != StateCancelling {
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
	case EventTextDelta:
		m.state = StateStreaming
		m.appendAgentDelta(event.Delta)
	case EventToolStart:
		m.state = StateExecutingTool
		if event.ToolCall != nil {
			m.timeline = append(m.timeline, timelineItem{kind: timelineTool, tool: &toolView{name: event.ToolCall.Name, callID: event.ToolCall.ID, arguments: string(event.ToolCall.Arguments), running: true}})
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
		if m.busy() && !m.cancelRequested {
			m.cancelRequested = true
			m.state = StateCancelling
			if m.cancel != nil {
				m.cancel()
			}
			return m, nil
		}
		return m, tea.Quit
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
	case StateConnecting, StateFetchingModels, StateWaitingFirstToken, StateStreaming, StateExecutingTool, StateAwaitingApproval, StateRetryBackoff, StateCancelling:
		return true
	default:
		return false
	}
}

func runBackendCmd(ctx context.Context, backend Backend, operationID, prompt string, events chan Event) tea.Cmd {
	return func() tea.Msg {
		err := backend.Submit(ctx, operationID, prompt, func(event Event) {
			if event.OperationID == "" {
				event.OperationID = operationID
			}
			events <- event
		})
		if err != nil {
			events <- Event{OperationID: operationID, Kind: EventNotice, Notice: err.Error(), Error: true}
			events <- Event{OperationID: operationID, Kind: EventState, State: StateFailed}
		} else {
			events <- Event{OperationID: operationID, Kind: EventState, State: StateCompleted}
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
			item.tool.running = false
			item.tool.content = result.Content
			item.tool.isError = result.IsError
			item.tool.truncated = result.Truncated
			return
		}
	}
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
