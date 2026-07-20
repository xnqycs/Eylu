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
	screenChat           screenKind = "chat"
	screenProviders      screenKind = "providers"
	screenProviderForm   screenKind = "provider_form"
	screenModels         screenKind = "models"
	screenContextConfirm screenKind = "context_confirm"
	screenSkills         screenKind = "skills"
	screenContext        screenKind = "context"
	screenTasks          screenKind = "tasks"
	screenToolDetail     screenKind = "tool_detail"
)

type timelineKind string

const (
	timelineMessage     timelineKind = "message"
	timelineTool        timelineKind = "tool"
	timelineNotice      timelineKind = "notice"
	maxInputRows                     = 8
	maxInputContentRows              = 10_000
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
	todoList             *protocol.TodoList
}

const approvedPlanImplementationPrompt = "Implement the approved plan now. Follow the plan already present in this conversation, inspect current files before editing, and run the relevant verification."

type planGateState struct {
	cursor   int
	feedback textinput.Model
	editing  bool
}

type askState struct {
	request    *AskRequest
	question   int
	cursor     int
	selections map[string]map[int]bool
	custom     map[string]string
	input      textinput.Model
	editing    bool
	err        string
}

type contextWindowConfirmState struct {
	selection ModelSelection
	cursor    int
	input     textinput.Model
	editing   bool
	err       string
}

func newContextWindowConfirmState(selection ModelSelection, width int) *contextWindowConfirmState {
	input := textinput.New()
	input.Placeholder = "Context window tokens"
	input.CharLimit = 12
	input.SetVirtualCursor(true)
	input.SetWidth(max(20, width-8))
	return &contextWindowConfirmState{selection: selection, input: input}
}

func newAskState(request *AskRequest, width int) *askState {
	input := textinput.New()
	input.Placeholder = "Type a custom answer"
	input.CharLimit = 1 << 12
	input.SetVirtualCursor(true)
	input.SetWidth(max(20, width-8))
	return &askState{
		request: request, selections: make(map[string]map[int]bool), custom: make(map[string]string), input: input,
	}
}

func newPlanGate(width int) *planGateState {
	feedback := textinput.New()
	feedback.Placeholder = "What should the next plan change?"
	feedback.CharLimit = 1 << 12
	feedback.SetVirtualCursor(true)
	feedback.SetWidth(max(20, width-12))
	return &planGateState{feedback: feedback}
}

type Model struct {
	backend Backend
	context context.Context
	clock   Clock
	styles  Styles

	input          textarea.Model
	viewport       viewport.Model
	spinner        spinner.Model
	modelFilter    textinput.Model
	form           providerFormModel
	completion     completionState
	approvalReason textinput.Model

	width  int
	height int
	screen screenKind
	state  OperationState

	snapshot                   Snapshot
	timeline                   []timelineItem
	providerCursor             int
	skillCursor                int
	modelCursor                int
	toolCursor                 int
	models                     []string
	modelManual                bool
	contextExpand              bool
	contextWindowConfirm       *contextWindowConfirmState
	approval                   *ApprovalRequest
	approvalCursor             int
	approvalEditing            bool
	ask                        *askState
	planGate                   *planGateState
	pendingImplementationMode  string
	restorePlanGateOnModeError bool
	files                      []FileItem
	filesLoaded                bool
	filesLoading               bool
	filesDiagnostic            string
	queuedMode                 string
	draft                      string
	selection                  selectionState
	clipboardWrite             func(string) error
	copyToast                  string
	copyToastSequence          uint64

	operationID          string
	operationMode        string
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

type modelSelectionMsg struct {
	selection ModelSelection
	returnTo  screenKind
	err       error
}

type contextWindowResultMsg struct {
	selection ModelSelection
	value     int
	err       error
}

type modelsResultMsg struct {
	models []string
	err    error
}

type modeResultMsg struct {
	previous string
	next     string
	err      error
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
	styles := DefaultStyles(options.NoColor)
	input := textarea.New()
	input.Placeholder = "Message Eylu"
	input.Prompt = "> "
	input.ShowLineNumbers = false
	input.DynamicHeight = true
	input.MinHeight = 1
	input.MaxHeight = maxInputRows
	input.MaxContentHeight = maxInputContentRows
	input.SetHeight(1)
	input.CharLimit = 1 << 20
	input.SetVirtualCursor(false)
	inputStyles := input.Styles()
	inputStyles.Cursor.Shape = tea.CursorBar
	inputStyles.Cursor.Blink = true
	inputStyles.Focused.Text = styles.Agent
	inputStyles.Focused.Prompt = styles.Accent
	inputStyles.Focused.Placeholder = styles.Muted
	inputStyles.Focused.EndOfBuffer = styles.Muted
	inputStyles.Blurred.Text = styles.Agent
	inputStyles.Blurred.Prompt = styles.Muted
	inputStyles.Blurred.Placeholder = styles.Muted
	inputStyles.Blurred.EndOfBuffer = styles.Muted
	input.SetStyles(inputStyles)
	input.SetWidth(width)
	filter := textinput.New()
	filter.Placeholder = "Filter models"
	filter.SetVirtualCursor(true)
	filter.SetWidth(max(20, width-6))
	approvalReason := textinput.New()
	approvalReason.Placeholder = "Reason for rejection"
	approvalReason.CharLimit = 1 << 12
	approvalReason.SetVirtualCursor(true)
	approvalReason.SetWidth(max(20, width-12))
	clipboardWrite := options.ClipboardWrite
	if clipboardWrite == nil {
		clipboardWrite = defaultClipboardWrite
	}
	model := &Model{
		backend: backend, context: baseContext, clock: clock, styles: styles, input: input,
		viewport: viewport.New(viewport.WithWidth(width), viewport.WithHeight(max(5, height-8))),
		spinner:  spinner.New(spinner.WithSpinner(spinner.Dot)), modelFilter: filter, approvalReason: approvalReason,
		width: width, height: height, screen: screenChat, state: StateIdle, followOutput: true,
		animation: !options.NoAnimation, noColor: options.NoColor, clipboardWrite: clipboardWrite,
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
			if m.snapshot.Workspace != typed.snapshot.Workspace {
				m.files = nil
				m.filesLoaded = false
				m.filesLoading = false
				m.filesDiagnostic = ""
			}
			m.snapshot = typed.snapshot
		}
		m.refreshViewport()
		return m, nil
	case filesResultMsg:
		m.filesLoading = false
		if typed.err != nil {
			m.filesLoaded = false
			m.filesDiagnostic = typed.err.Error()
			m.appendNotice(typed.err.Error(), true)
		} else {
			m.files = append(m.files[:0], typed.files...)
			m.filesLoaded = true
			m.filesDiagnostic = ""
		}
		return m, m.refreshCompletion()
	case clipboardResultMsg:
		return m, m.handleClipboardResult(typed)
	case copyToastExpiredMsg:
		if typed.sequence == m.copyToastSequence {
			m.copyToast = ""
		}
		return m, nil
	case modeResultMsg:
		if typed.err != nil {
			m.snapshot.Mode = typed.previous
			m.pendingImplementationMode = ""
			if m.restorePlanGateOnModeError {
				m.planGate = newPlanGate(m.width)
			}
			m.restorePlanGateOnModeError = false
			m.appendNotice(typed.err.Error(), true)
		} else {
			m.snapshot.Mode = typed.next
			m.restorePlanGateOnModeError = false
			if m.pendingImplementationMode == typed.next {
				m.pendingImplementationMode = ""
				_, startCommand := m.startRequest(Submission{Text: approvedPlanImplementationPrompt})
				return m, tea.Batch(m.loadSnapshotCmd(), startCommand)
			}
		}
		return m, m.loadSnapshotCmd()
	case backendEventMsg:
		return m.handleBackendEvent(typed.event)
	case backendEventClosedMsg:
		if typed.operationID == m.operationID {
			if m.state == StateCancelling {
				m.state = StateCancelled
			} else if m.state != StateFailed && m.state != StateCancelled && m.state != StateInterrupted {
				m.state = StateCompleted
			}
			m.cancel = nil
			m.cancelRequested = false
			m.eventChannel = nil
			m.ask = nil
			if m.state == StateFailed && strings.TrimSpace(m.input.Value()) == "" && m.draft != "" {
				m.input.SetValue(m.draft)
				m.input.MoveToEnd()
				m.updateViewportHeight()
			} else if m.state == StateCompleted {
				m.draft = ""
				if m.operationMode == "plan" {
					m.queuedMode = ""
					m.planGate = newPlanGate(m.width)
				}
			}
		}
		commands := []tea.Cmd{m.loadSnapshotCmd()}
		if m.queuedMode != "" && m.planGate == nil {
			next := m.queuedMode
			m.queuedMode = ""
			commands = append(commands, m.setModeCmd(next))
		}
		return m, tea.Batch(commands...)
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
	case modelSelectionMsg:
		m.state = StateIdle
		if typed.err != nil {
			if typed.returnTo == screenProviderForm {
				m.form.err = typed.err
				m.screen = screenProviderForm
			} else {
				m.appendNotice(typed.err.Error(), true)
				m.screen = screenChat
				m.refreshViewport()
			}
			return m, nil
		}
		m.modelFilter.Blur()
		m.contextWindowConfirm = newContextWindowConfirmState(typed.selection, m.viewportContentWidth())
		if typed.selection.DetectedContextWindow <= 0 {
			m.contextWindowConfirm.cursor = 1
			m.contextWindowConfirm.err = "No context window was detected; enter the correct value."
		}
		m.screen = screenContextConfirm
		return m, m.loadSnapshotCmd()
	case contextWindowResultMsg:
		if typed.err != nil {
			if m.contextWindowConfirm != nil {
				m.contextWindowConfirm.err = typed.err.Error()
			}
			return m, nil
		}
		m.appendNotice(fmt.Sprintf("Model: %s · context window: %d", typed.selection.Model, typed.value), false)
		m.contextWindowConfirm = nil
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
	case tea.MouseMsg:
		return m.handleMouse(typed)
	case interruptRequestMsg:
		return m.handleInterrupt()
	case tea.PasteMsg:
		if m.ask != nil && m.ask.editing {
			updated, command := m.ask.input.Update(message)
			m.ask.input = updated
			return m, command
		}
		if m.approval != nil && m.approvalEditing {
			updated, command := m.approvalReason.Update(message)
			m.approvalReason = updated
			return m, command
		}
		if m.planGate != nil && m.planGate.editing {
			updated, command := m.planGate.feedback.Update(message)
			m.planGate.feedback = updated
			return m, command
		}
		if m.screen == screenChat {
			m.clearSelection()
			updated, command := m.input.Update(message)
			m.input = updated
			completionCommand := m.refreshCompletion()
			return m, tea.Batch(command, completionCommand)
		}
	}
	if m.screen == screenChat {
		updated, command := m.input.Update(message)
		m.input = updated
		completionCommand := m.refreshCompletion()
		return m, tea.Batch(command, completionCommand)
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
			if event.ToolResult.TodoList != nil && !event.ToolResult.IsError {
				m.snapshot.TodoList = cloneUITodoList(*event.ToolResult.TodoList)
			}
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
		m.approvalCursor = 0
		m.approvalEditing = false
		m.approvalReason.Reset()
		m.state = StateAwaitingApproval
	case EventAsk:
		m.ask = newAskState(event.Ask, m.width)
		m.state = StateAwaitingInput
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
	if m.eventChannel != nil && event.Kind != EventApproval && event.Kind != EventAsk {
		command = tea.Batch(command, waitEventCmd(m.operationID, m.eventChannel))
	}
	return m, command
}

func (m *Model) handleKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	m.clearSelection()
	if key == "ctrl+c" {
		return m.handleInterrupt()
	}
	if m.ask != nil {
		if m.ask.editing && key != "enter" && key != "esc" && key != "tab" {
			updated, command := m.ask.input.Update(message)
			m.ask.input = updated
			return m, command
		}
		return m.handleAskKey(key)
	}
	if m.approval != nil {
		if m.approvalEditing && key != "enter" && key != "esc" && key != "tab" {
			updated, command := m.approvalReason.Update(message)
			m.approvalReason = updated
			return m, command
		}
		return m.handleApprovalKey(key)
	}
	if m.planGate != nil {
		if m.planGate.editing && key != "enter" && key != "esc" && key != "tab" {
			updated, command := m.planGate.feedback.Update(message)
			m.planGate.feedback = updated
			return m, command
		}
		return m.handlePlanGateKey(key)
	}
	switch m.screen {
	case screenProviders:
		return m.handleProvidersKey(key)
	case screenProviderForm:
		return m.handleProviderFormKey(message)
	case screenModels:
		return m.handleModelsKey(message)
	case screenContextConfirm:
		return m.handleContextWindowConfirmKey(message)
	case screenSkills:
		return m.handleSkillsKey(key)
	case screenContext:
		if key == "esc" {
			m.screen = screenChat
		} else if key == "enter" {
			m.contextExpand = !m.contextExpand
		}
		return m, nil
	case screenTasks:
		if key == "esc" {
			m.screen = screenChat
			m.refreshViewport()
			return m, nil
		}
		updated, command := m.viewport.Update(message)
		m.viewport = updated
		return m, command
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
		var resume tea.Cmd
		if m.ask != nil {
			resume = m.sendAskDecision(AskDecision{Cancelled: true})
		}
		m.cancelRequested = true
		m.state = StateCancelling
		if m.cancel != nil {
			m.cancel()
		}
		m.appendNotice("Cancellation requested.", false)
		m.refreshViewport()
		return m, resume
	}
	return m, tea.Quit
}

func (m *Model) handleChatKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	if key != "shift+enter" && key != "ctrl+enter" {
		if handled, command := m.handleCompletionKey(key); handled {
			return m, command
		}
	}
	switch key {
	case "enter":
		value := strings.TrimSpace(m.input.Value())
		if value == "" {
			return m, nil
		}
		if strings.HasPrefix(value, "/") {
			m.input.Reset()
			m.completion = completionState{}
			m.updateViewportHeight()
			return m.executeSlash(value)
		}
		if m.busy() {
			m.appendNotice("A request is already running.", true)
			m.refreshViewport()
			return m, nil
		}
		return m.startRequest(Submission{Text: value, References: parseReferences(value)})
	case "shift+enter", "ctrl+enter":
		m.input.InsertString("\n")
		return m, m.refreshCompletion()
	case "shift+tab":
		return m.cycleMode()
	case "pgup":
		m.completion = completionState{}
		m.followOutput = false
		m.viewport.PageUp()
		m.updateViewportHeight()
		return m, nil
	case "pgdown":
		m.completion = completionState{}
		m.viewport.PageDown()
		m.followOutput = m.viewport.AtBottom()
		m.updateViewportHeight()
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
	completionCommand := m.refreshCompletion()
	return m, tea.Batch(command, completionCommand)
}

func (m *Model) startRequest(submission Submission) (tea.Model, tea.Cmd) {
	if m.busy() {
		m.appendNotice("A request is already running.", true)
		m.refreshViewport()
		return m, nil
	}
	sequence := atomic.AddUint64(&m.operationSeq, 1)
	m.operationID = fmt.Sprintf("op-%d", sequence)
	m.operationMode = m.snapshot.Mode
	if m.operationMode == "" {
		m.operationMode = "manual"
	}
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
	m.draft = submission.Text
	m.input.Reset()
	m.completion = completionState{}
	m.updateViewportHeight()
	m.timeline = append(m.timeline, timelineItem{kind: timelineMessage, role: "user", text: submission.Text})
	m.followOutput = true
	m.refreshViewport()
	commands := []tea.Cmd{runBackendCmd(requestContext, m.backend, m.operationID, submission, m.eventChannel), waitEventCmd(m.operationID, m.eventChannel)}
	if m.animation {
		commands = append(commands, wrapOperationCmd(m.operationID, func() tea.Msg { return m.spinner.Tick() }))
	}
	return m, tea.Batch(commands...)
}

func (m *Model) executeSlash(line string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(line)
	command := fields[0]
	for _, item := range m.snapshot.Skills {
		if item.Status == "active" && command == "/"+item.Name {
			line = "/skill " + item.Name
			fields = strings.Fields(line)
			command = fields[0]
			break
		}
	}
	switch command {
	case "/quit":
		return m, tea.Quit
	case "/context":
		m.screen = screenContext
		return m, m.loadSnapshotCmd()
	case "/tasks":
		m.screen = screenTasks
		m.refreshViewport()
		return m, m.loadSnapshotCmd()
	case "/skills":
		m.screen = screenSkills
		return m, m.loadSnapshotCmd()
	case "/providers":
		m.screen = screenProviders
		return m, m.loadSnapshotCmd()
	case "/provider":
		if len(fields) >= 2 && fields[1] == "add" {
			m.form = newProviderFormModel(ProviderForm{Adapter: "openai_responses"}, m.viewportContentWidth())
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
		if len(fields) == 2 {
			return m, m.selectModelCmd(m.snapshot.Provider, fields[1], screenChat)
		}
	case "/mode":
		if len(fields) == 2 {
			return m.requestMode(fields[1])
		}
	case "/help":
		m.appendNotice("/new /tasks /context /skills /skill /providers /provider /model /mode /quit  ·  Shift+Tab cycles mode  ·  Plan: Auto/Full/Reject  ·  Approval: Tab adds rejection feedback", false)
		m.refreshViewport()
		return m, nil
	}
	return m, m.commandCmd(line)
}

func (m *Model) cycleMode() (tea.Model, tea.Cmd) {
	current := m.snapshot.Mode
	if m.queuedMode != "" {
		current = m.queuedMode
	}
	modes := []string{"manual", "plan", "auto", "full"}
	next := modes[0]
	for index, mode := range modes {
		if mode == current {
			next = modes[(index+1)%len(modes)]
			break
		}
	}
	return m.requestMode(next)
}

func (m *Model) requestMode(next string) (tea.Model, tea.Cmd) {
	switch next {
	case "manual", "plan", "auto", "full":
	default:
		m.appendNotice("unknown permission mode "+next, true)
		return m, nil
	}
	if m.busy() {
		m.queuedMode = next
		return m, nil
	}
	return m, m.setModeCmd(next)
}

func (m *Model) setModeCmd(next string) tea.Cmd {
	previous := m.snapshot.Mode
	if previous == "" {
		previous = "manual"
	}
	m.snapshot.Mode = next
	return func() tea.Msg {
		err := m.backend.SetMode(m.context, next)
		return modeResultMsg{previous: previous, next: next, err: err}
	}
}

func (m *Model) handleApprovalKey(key string) (tea.Model, tea.Cmd) {
	if m.approvalEditing {
		switch strings.ToLower(key) {
		case "enter":
			return m, m.submitApproval(ApprovalDecision{Reason: strings.TrimSpace(m.approvalReason.Value())})
		case "esc", "tab":
			m.approvalEditing = false
			m.approvalReason.Blur()
			return m, nil
		default:
			return m, nil
		}
	}
	switch strings.ToLower(key) {
	case "up", "left":
		m.approvalCursor = max(0, m.approvalCursor-1)
		return m, nil
	case "down", "right":
		m.approvalCursor = min(1, m.approvalCursor+1)
		return m, nil
	case "tab":
		m.approvalCursor = 1
		m.approvalEditing = true
		return m, m.approvalReason.Focus()
	case "y":
		return m, m.submitApproval(ApprovalDecision{Approved: true})
	case "n", "esc":
		return m, m.submitApproval(ApprovalDecision{})
	case "enter":
		if m.approvalCursor == 0 {
			return m, m.submitApproval(ApprovalDecision{Approved: true})
		}
		return m, m.submitApproval(ApprovalDecision{Reason: strings.TrimSpace(m.approvalReason.Value())})
	default:
		return m, nil
	}
}

func (m *Model) submitApproval(decision ApprovalDecision) tea.Cmd {
	select {
	case m.approval.Response <- decision:
	default:
	}
	m.approval = nil
	m.approvalEditing = false
	m.approvalReason.Blur()
	m.approvalReason.Reset()
	m.state = StateExecutingTool
	return waitEventCmd(m.operationID, m.eventChannel)
}

func (m *Model) handleAskKey(key string) (tea.Model, tea.Cmd) {
	if m.ask == nil || m.ask.request == nil || len(m.ask.request.Questions) == 0 {
		return m, nil
	}
	question := m.ask.request.Questions[m.ask.question]
	if m.ask.editing {
		switch strings.ToLower(key) {
		case "enter":
			value := strings.TrimSpace(m.ask.input.Value())
			if value == "" {
				m.ask.err = "Custom answer is required."
				return m, nil
			}
			m.ask.custom[question.ID] = value
			m.ask.editing = false
			m.ask.input.Blur()
			return m.advanceAsk()
		case "tab":
			m.ask.custom[question.ID] = strings.TrimSpace(m.ask.input.Value())
			m.ask.editing = false
			m.ask.input.Blur()
			return m, nil
		case "esc":
			return m.cancelAsk()
		default:
			return m, nil
		}
	}
	optionCount := len(question.Options) + 1
	switch strings.ToLower(key) {
	case "up":
		m.ask.cursor = max(0, m.ask.cursor-1)
	case "down":
		m.ask.cursor = min(optionCount-1, m.ask.cursor+1)
	case "left":
		if m.ask.question > 0 {
			m.ask.question--
			m.restoreAskCursor()
		}
	case "right":
		if m.ask.question+1 < len(m.ask.request.Questions) && m.askQuestionAnswered(question) {
			m.ask.question++
			m.restoreAskCursor()
		}
	case "tab":
		if !question.Multiple {
			m.ask.selections[question.ID] = make(map[int]bool)
		}
		m.ask.cursor = len(question.Options)
		m.ask.input.SetValue(m.ask.custom[question.ID])
		m.ask.input.CursorEnd()
		m.ask.editing = true
		m.ask.err = ""
		return m, m.ask.input.Focus()
	case "space":
		if m.ask.cursor == len(question.Options) {
			m.ask.input.SetValue(m.ask.custom[question.ID])
			m.ask.input.CursorEnd()
			m.ask.editing = true
			m.ask.err = ""
			return m, m.ask.input.Focus()
		}
		if question.Multiple {
			selected := m.selectionsFor(question.ID)
			selected[m.ask.cursor] = !selected[m.ask.cursor]
			m.ask.err = ""
		}
	case "enter":
		if m.ask.cursor == len(question.Options) {
			m.ask.input.SetValue(m.ask.custom[question.ID])
			m.ask.input.CursorEnd()
			m.ask.editing = true
			m.ask.err = ""
			return m, m.ask.input.Focus()
		}
		selected := m.selectionsFor(question.ID)
		if question.Multiple {
			if len(m.askAnswers(question)) == 0 {
				m.ask.err = "Select at least one answer."
				return m, nil
			}
		} else {
			clear(selected)
			selected[m.ask.cursor] = true
			delete(m.ask.custom, question.ID)
		}
		return m.advanceAsk()
	case "esc":
		return m.cancelAsk()
	}
	return m, nil
}

func (m *Model) selectionsFor(id string) map[int]bool {
	selected := m.ask.selections[id]
	if selected == nil {
		selected = make(map[int]bool)
		m.ask.selections[id] = selected
	}
	return selected
}

func (m *Model) askQuestionAnswered(question protocol.AskQuestion) bool {
	return len(m.askAnswers(question)) > 0
}

func (m *Model) askAnswers(question protocol.AskQuestion) []string {
	answers := make([]string, 0, len(question.Options)+1)
	selected := m.ask.selections[question.ID]
	for index, option := range question.Options {
		if selected[index] {
			answers = append(answers, option.Label)
		}
	}
	if custom := strings.TrimSpace(m.ask.custom[question.ID]); custom != "" {
		answers = append(answers, custom)
	}
	return answers
}

func (m *Model) advanceAsk() (tea.Model, tea.Cmd) {
	question := m.ask.request.Questions[m.ask.question]
	if len(m.askAnswers(question)) == 0 {
		m.ask.err = "Select at least one answer."
		return m, nil
	}
	if m.ask.question+1 < len(m.ask.request.Questions) {
		m.ask.question++
		m.restoreAskCursor()
		return m, nil
	}
	answers := make(map[string][]string, len(m.ask.request.Questions))
	for _, item := range m.ask.request.Questions {
		values := m.askAnswers(item)
		if len(values) == 0 {
			m.ask.err = "Answer every question before submitting."
			return m, nil
		}
		answers[item.ID] = values
	}
	return m, m.sendAskDecision(AskDecision{Answers: answers})
}

func (m *Model) restoreAskCursor() {
	question := m.ask.request.Questions[m.ask.question]
	m.ask.cursor = 0
	for index := range question.Options {
		if m.ask.selections[question.ID][index] {
			m.ask.cursor = index
			break
		}
	}
	if strings.TrimSpace(m.ask.custom[question.ID]) != "" && len(m.ask.selections[question.ID]) == 0 {
		m.ask.cursor = len(question.Options)
	}
	m.ask.err = ""
}

func (m *Model) cancelAsk() (tea.Model, tea.Cmd) {
	return m, m.sendAskDecision(AskDecision{Cancelled: true})
}

func (m *Model) sendAskDecision(decision AskDecision) tea.Cmd {
	if m.ask == nil || m.ask.request == nil {
		return nil
	}
	select {
	case m.ask.request.Response <- decision:
	default:
	}
	m.ask.input.Blur()
	m.ask = nil
	m.state = StateExecutingTool
	return waitEventCmd(m.operationID, m.eventChannel)
}

func (m *Model) handlePlanGateKey(key string) (tea.Model, tea.Cmd) {
	if m.planGate == nil {
		return m, nil
	}
	if m.planGate.editing {
		switch strings.ToLower(key) {
		case "enter":
			feedback := strings.TrimSpace(m.planGate.feedback.Value())
			if feedback == "" {
				return m, nil
			}
			m.planGate = nil
			prompt := "Revise the current implementation plan using this user feedback:\n\n" + feedback
			return m.startRequest(Submission{Text: prompt})
		case "esc", "tab":
			m.planGate.editing = false
			m.planGate.feedback.Blur()
			return m, nil
		default:
			return m, nil
		}
	}
	switch strings.ToLower(key) {
	case "left", "up":
		m.planGate.cursor = max(0, m.planGate.cursor-1)
		return m, nil
	case "right", "down":
		m.planGate.cursor = min(2, m.planGate.cursor+1)
		return m, nil
	case "tab":
		m.planGate.cursor = 2
		m.planGate.editing = true
		return m, m.planGate.feedback.Focus()
	case "enter":
		switch m.planGate.cursor {
		case 0:
			m.planGate = nil
			m.pendingImplementationMode = "auto"
			m.restorePlanGateOnModeError = true
			return m.requestMode("auto")
		case 1:
			m.planGate = nil
			m.pendingImplementationMode = "full"
			m.restorePlanGateOnModeError = true
			return m.requestMode("full")
		default:
			m.planGate = nil
			m.restorePlanGateOnModeError = true
			return m.requestMode("manual")
		}
	case "esc":
		m.planGate = nil
		m.restorePlanGateOnModeError = true
		return m.requestMode("manual")
	default:
		return m, nil
	}
}

func (m *Model) busy() bool {
	switch m.state {
	case StateConnecting, StateFetchingModels, StateWaitingFirstToken, StateStreaming, StatePreparingTool, StateExecutingTool, StateAwaitingApproval, StateAwaitingInput, StateRetryBackoff, StateCancelling:
		return true
	default:
		return false
	}
}

func runBackendCmd(ctx context.Context, backend Backend, operationID string, submission Submission, events chan Event) tea.Cmd {
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
		err := backend.Submit(ctx, operationID, submission, func(event Event) {
			if event.OperationID == "" {
				event.OperationID = operationID
			}
			enqueue(event, event.Kind == EventToolCallDelta)
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				enqueue(Event{OperationID: operationID, Kind: EventNotice, Notice: "Request cancelled."}, false)
				enqueue(Event{OperationID: operationID, Kind: EventState, State: StateCancelled}, false)
			} else if errors.Is(err, ErrRequestInterrupted) {
				enqueue(Event{OperationID: operationID, Kind: EventState, State: StateInterrupted}, false)
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
			if result.TodoList != nil {
				list := cloneUITodoList(*result.TodoList)
				item.tool.todoList = &list
			}
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

func cloneUITodoList(list protocol.TodoList) protocol.TodoList {
	return protocol.TodoList{Explanation: list.Explanation, Items: append([]protocol.TodoItem(nil), list.Items...)}
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

func (m *Model) lastToolIndex() int {
	for index := len(m.timeline) - 1; index >= 0; index-- {
		if m.timeline[index].kind == timelineTool {
			return index
		}
	}
	return -1
}
