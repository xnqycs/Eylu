package ui

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	contextledger "Eylu/internal/context"
	"Eylu/internal/protocol"
)

type markdownRenderCache struct {
	renderer *glamour.TermRenderer
	width    int
}

const (
	activityGapRows        = 1
	fixedChromeRows        = 5 + activityGapRows
	minViewportRows        = 4
	maxTaskPanelItems      = 5
	bannerViewportRows     = 8
	colorAnimationInterval = 50 * time.Millisecond
	colorSpatialFrequency  = 0.24
	colorTemporalFrequency = 2.0
)

type tuiLayout struct {
	viewportTop     int
	viewportHeight  int
	completionRows  int
	taskRows        int
	inputContentRow int
	panelHeight     int
}

func (m *Model) layout() tuiLayout {
	layout := tuiLayout{viewportTop: 1}
	if m.approval != nil || m.ask != nil || m.planGate != nil {
		layout.panelHeight = m.decisionPanelHeight()
		layout.viewportHeight = max(minViewportRows, m.height-layout.viewportTop-layout.panelHeight)
		return layout
	}
	layout.taskRows = m.taskPanelRows()
	layout.completionRows = m.completionHeight()
	layout.viewportHeight = max(minViewportRows, m.height-m.input.Height()-fixedChromeRows-layout.completionRows-layout.taskRows)
	layout.inputContentRow = layout.viewportTop + layout.viewportHeight + layout.completionRows + activityGapRows + 2 + layout.taskRows
	return layout
}

func (m *Model) decisionPanelHeight() int {
	if m.ask != nil {
		desired := max(8, min(12, m.height/2))
		maximum := max(1, m.height-1-minViewportRows)
		return min(desired, maximum)
	}
	desired := max(6, m.height/3)
	maximum := max(1, m.height-1-minViewportRows)
	return min(desired, maximum)
}

func (m *Model) View() tea.View {
	if m.screen != screenChat {
		m.refreshViewport()
	}
	layout := m.layout()
	m.setViewportHeight(layout.viewportHeight)
	header := m.renderHeader()
	parts := []string{header, m.renderViewport()}
	if m.approval != nil {
		parts = append(parts, m.renderApproval(layout.panelHeight))
	} else if m.ask != nil {
		parts = append(parts, m.renderAsk(layout.panelHeight))
	} else if m.planGate != nil {
		parts = append(parts, m.renderPlanGate(layout.panelHeight))
	} else {
		completion := m.renderCompletion()
		if completion != "" {
			parts = append(parts, completion)
		}
		input := strings.Repeat("\n", 2)
		if m.screen == screenChat {
			input = m.renderInputBand()
		}
		parts = append(parts, renderBlankRows(m.width, activityGapRows), m.renderLoading())
		if layout.taskRows > 0 {
			parts = append(parts, m.renderTaskPanel(layout.taskRows))
		}
		parts = append(parts, input, m.renderStatus())
	}
	content := fitRenderedRows(strings.Join(parts, "\n"), m.height)
	if m.noColor {
		content = ansi.Strip(content)
	}
	view := tea.NewView(content)
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	view.WindowTitle = "Eylu"
	view.KeyboardEnhancements.ReportAllKeysAsEscapeCodes = true
	view.KeyboardEnhancements.ReportAssociatedText = true
	if m.screen == screenChat && m.approval == nil && m.ask == nil && m.planGate == nil {
		view.Cursor = m.input.Cursor()
		if view.Cursor != nil {
			localRow := min(max(0, view.Cursor.Position.Y), max(0, m.input.Height()-1))
			view.Cursor.Position.Y = layout.inputContentRow + localRow
		}
	}
	return view
}

func (m *Model) renderInputBand() string {
	rule := m.styles.InputBorder.Render(strings.Repeat("─", m.width))
	input := strings.TrimRight(m.input.View(), "\n")
	return strings.Join([]string{rule, input, rule}, "\n")
}

func renderBlankRows(width, rows int) string {
	if rows <= 0 {
		return ""
	}
	line := strings.Repeat(" ", max(0, width))
	lines := make([]string, rows)
	for index := range lines {
		lines[index] = line
	}
	return strings.Join(lines, "\n")
}

func (m *Model) resize(width, height int) {
	m.width = max(40, width)
	m.height = max(12, height)
	m.input.SetWidth(m.width)
	m.modelFilter.SetWidth(max(20, m.viewportContentWidth()-6))
	m.approvalReason.SetWidth(max(20, m.width-12))
	if m.ask != nil {
		m.ask.input.SetWidth(max(20, m.width-8))
	}
	if m.planGate != nil {
		m.planGate.feedback.SetWidth(max(20, m.width-12))
	}
	if m.contextWindowConfirm != nil {
		m.contextWindowConfirm.input.SetWidth(max(20, m.viewportContentWidth()-8))
	}
	m.form.setWidth(m.viewportContentWidth())
	m.viewport.SetWidth(m.viewportContentWidth())
	m.updateViewportHeight()
	m.refreshViewport()
}

func (m *Model) updateViewportHeight() {
	reservedCompletion := 0
	if m.completion.kind != completionNone {
		reservedCompletion = 1
	}
	inputLimit := min(maxInputRows, max(1, m.height-fixedChromeRows-minViewportRows-reservedCompletion-m.taskPanelRows()))
	if m.input.MaxHeight != inputLimit {
		m.input.MaxHeight = inputLimit
		m.input.SetWidth(m.width)
	}
	m.setViewportHeight(m.layout().viewportHeight)
}

func (m *Model) setViewportHeight(height int) {
	if m.viewport.Height() == height {
		return
	}
	wasBottom := m.viewport.AtBottom() || m.followOutput
	m.viewport.SetHeight(height)
	if wasBottom {
		m.viewport.GotoBottom()
	}
}

func (m *Model) refreshViewport() {
	wasBottom := m.viewport.AtBottom() || (m.followOutput && m.screen == screenChat)
	var content string
	switch m.screen {
	case screenProviders:
		content = m.renderProviders()
	case screenProviderForm:
		content = m.form.view(m.styles)
	case screenModels:
		content = m.renderModels()
	case screenContextConfirm:
		content = m.renderContextWindowConfirmation()
	case screenSkills:
		content = m.renderSkills()
	case screenMCP:
		content = m.renderMCPServers()
	case screenMCPDetail:
		content = m.renderMCPDetail()
	case screenMCPToolDetail:
		content = m.renderMCPToolDetail()
	case screenContext:
		content = m.renderContext()
	case screenTasks:
		content = m.renderTasks()
	case screenToolDetail:
		content = m.renderToolDetail()
	default:
		content = m.renderTimeline()
	}
	m.viewport.SetContent(content)
	if wasBottom {
		m.viewport.GotoBottom()
	}
}

func (m *Model) renderHeader() string {
	provider := m.snapshot.Provider
	if provider == "" {
		provider = "unconfigured"
	}
	model := m.snapshot.Model
	if model == "" {
		model = "no model"
	}
	effort := m.snapshot.ReasoningEffort
	if effort == "" {
		effort = "auto"
	}
	title := m.styles.Header.Render("Eylu")
	available := max(0, m.width-lipgloss.Width(title)-1)
	effort = truncateColumns(effort, available)
	coreAvailable := max(0, available-lipgloss.Width(effort)-2)
	core := truncateColumns(fmt.Sprintf("%s  %s", provider, model), coreAvailable)
	meta := m.styles.Active.Render(effort)
	if core != "" {
		meta = m.styles.Status.Render(core+"  ") + meta
	}
	space := max(1, m.width-lipgloss.Width(title)-lipgloss.Width(meta))
	return truncateColumns(title+strings.Repeat(" ", space)+meta, m.width)
}

func (m *Model) renderLoading() string {
	left := ""
	if m.busy() {
		left = m.renderActivityLine()
	}
	toast := ""
	if m.copyToast != "" {
		toast = m.styles.Active.Render(m.copyToast)
	}
	if toast == "" {
		return padWidth(ansi.Truncate(left, m.width, "..."), m.width)
	}
	available := max(0, m.width-lipgloss.Width(toast)-1)
	left = ansi.Truncate(left, available, "...")
	space := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(toast))
	return left + strings.Repeat(" ", space) + toast
}

func (m *Model) renderActivityLine() string {
	prefix := "*"
	if m.animation {
		prefix = m.spinner.View()
	}
	startedAt := m.startedAt
	if m.state == StateCompacting && !m.compactionStartedAt.IsZero() {
		startedAt = m.compactionStartedAt
	}
	elapsed := time.Duration(0)
	if !startedAt.IsZero() {
		elapsed = max(time.Duration(0), m.clock.Now().Sub(startedAt)).Round(time.Second)
	}
	details := []string{elapsed.String()}
	if m.state != StateCompacting {
		details = append(details, m.renderInputActivity(), m.renderTokenActivity())
	}
	if m.state != StateCompacting {
		if thinking := m.renderThinkingActivity(); thinking != "" {
			details = append(details, thinking)
		}
	}
	if m.state == StateRetryBackoff && !m.retryAt.IsZero() {
		remaining := max(time.Duration(0), m.retryAt.Sub(m.clock.Now())).Round(100 * time.Millisecond)
		details = append(details, fmt.Sprintf("next in %s", remaining))
	}
	lead := m.styles.Loading.Render(fmt.Sprintf("%s %s...", prefix, activityLabel(m.state)))
	metadata := m.styles.Status.Render(fmt.Sprintf("(%s)", strings.Join(details, " · ")))
	return lead + "  " + metadata
}

func (m *Model) renderInputActivity() string {
	marker := ""
	if m.activity.InputTokens > 0 && !m.activity.InputExact {
		marker = "≈"
	}
	return fmt.Sprintf("↑ %s%d sent", marker, m.activity.InputTokens)
}

func (m *Model) renderTokenActivity() string {
	bytesPerToken := m.activity.TokenBytesPerToken
	if bytesPerToken <= 0 {
		bytesPerToken = 4
	}
	estimatedTokens := 0
	if m.streamedBytes > 0 {
		estimatedTokens = (m.streamedBytes + bytesPerToken - 1) / bytesPerToken
	}
	tokens := m.operationUsage.OutputTokens + estimatedTokens
	estimated := estimatedTokens > 0 || (tokens > 0 && !m.operationUsage.Exact)
	marker := ""
	if estimated {
		marker = "≈"
	}
	return fmt.Sprintf("↓ %s%d received", marker, tokens)
}

func (m *Model) renderThinkingActivity() string {
	if m.reasoningActive {
		return "thinking"
	}
	if m.reasoningSeen {
		duration := m.reasoningElapsed.Round(time.Second)
		if duration < time.Second {
			duration = time.Second
		}
		return "thought for " + duration.String()
	}
	return ""
}

func (m *Model) renderStatus() string {
	mode := m.snapshot.Mode
	if mode == "" {
		mode = "manual"
	}
	if m.queuedMode != "" {
		mode += " -> " + m.queuedMode
	}
	report := m.displayContextReport()
	fullLeft := fmt.Sprintf("%s · Context %s tokens", mode, formatTokenCount(report.TotalTokens))
	compactLeft := fullLeft
	if report.LimitKnown && report.ContextWindow > 0 {
		used := roundedContextPercent(report)
		fullLeft = fmt.Sprintf("%s · Context %d%% left · Context %d%% used", mode, 100-used, used)
		compactLeft = fmt.Sprintf("%s · %d%% left · %d%% used", mode, 100-used, used)
	}
	inset := m.viewportLeftInset()
	available := max(0, m.width-inset)
	right := statusHint(m.state)
	left := fullLeft
	if lipgloss.Width(fullLeft)+1+lipgloss.Width(right) > available && lipgloss.Width(compactLeft) < lipgloss.Width(fullLeft) {
		left = compactLeft
	}
	left = truncateColumns(left, available)
	rightAvailable := available - lipgloss.Width(left) - 1
	if rightAvailable < 8 {
		right = ""
	} else {
		right = truncateColumns(right, rightAvailable)
	}
	if m.colorAnimationEnabled() {
		line := strings.Repeat(" ", inset) + left
		if right != "" {
			gap := max(1, m.width-lipgloss.Width(line)-lipgloss.Width(right))
			line += strings.Repeat(" ", gap) + right
		}
		return renderThemeGradient(padWidth(line, m.width), m.colorAnimationElapsed, false)
	}
	line := strings.Repeat(" ", inset) + m.styles.Status.Render(left)
	if right != "" {
		gap := max(1, m.width-lipgloss.Width(line)-lipgloss.Width(right))
		line += strings.Repeat(" ", gap) + m.renderStatusHint(right)
	}
	return padWidth(line, m.width)
}

func (m *Model) renderTimeline() string {
	var output strings.Builder
	contentWidth := m.viewportContentWidth()
	output.WriteString(m.renderBanner())
	if status := m.renderMCPStartupStatus(); status != "" {
		output.WriteByte('\n')
		output.WriteString(status)
	}
	if len(m.timeline) > 0 {
		output.WriteString("\n\n")
	}
	for index := range m.timeline {
		item := &m.timeline[index]
		switch item.kind {
		case timelineMessage:
			if item.role == "user" {
				fmt.Fprintf(&output, "%s\n%s\n\n", m.styles.User.Render("YOU"), wrapPlain(item.text, max(1, contentWidth-2)))
			} else {
				fmt.Fprintf(&output, "%s\n%s\n\n", m.styles.Agent.Render("EYLU"), m.renderTimelineMarkdown(item))
			}
		case timelineTool:
			if item.tool != nil && item.tool.name == "todolist" {
				continue
			}
			fmt.Fprintf(&output, "%s\n", m.renderTool(item.tool))
			if next, ok := m.nextVisibleTimelineKind(index); ok && next != timelineTool {
				output.WriteByte('\n')
			}
		case timelineNotice:
			style := m.styles.Status
			if item.err {
				style = m.styles.Error
			}
			fmt.Fprintf(&output, "%s\n\n", style.Render(wrapPlain(item.text, max(1, contentWidth-2))))
		}
	}
	timeline := strings.TrimRight(output.String(), "\n")
	if m.inlineTaskPanelVisible() {
		if timeline != "" {
			timeline += "\n\n"
		}
		timeline += m.renderInlineTaskPanel()
	}
	return timeline
}

func (m *Model) renderMCPStartupStatus() string {
	if !m.mcpLoading {
		return ""
	}
	prefix := "*"
	if m.animation {
		prefix = m.mcpSpinner.View()
	}
	return m.styles.Loading.Render(truncateColumns(prefix+" MCP  Loading servers...", m.viewportContentWidth()))
}

func (m *Model) nextVisibleTimelineKind(index int) (timelineKind, bool) {
	for next := index + 1; next < len(m.timeline); next++ {
		item := m.timeline[next]
		if item.kind == timelineTool && (item.tool == nil || item.tool.name == "todolist") {
			continue
		}
		return item.kind, true
	}
	return "", false
}

func (m *Model) renderBanner() string {
	const art = "    ______   __  __    __    __  __\n" +
		"   / ____/  / / / /   / /   / / / /\n" +
		"  / __/    / /_/ /   / /   / / / /\n" +
		" / /___    \\__, /   / /   / /_/ /\n" +
		"/_____/    /____/   /_/    \\__,_/"
	workspace := m.workspace
	if m.snapshot.Workspace != "" {
		workspace = m.snapshot.Workspace
	}
	if workspace == "" {
		workspace = "."
	}
	workspace = filepath.ToSlash(filepath.Clean(workspace))
	version := strings.TrimSpace(m.version)
	if version == "" {
		version = "dev"
	} else if version != "dev" && !strings.HasPrefix(strings.ToLower(version), "v") {
		version = "v" + version
	}
	separator := "  ·  "
	pathWidth := max(0, m.viewportContentWidth()-lipgloss.Width(version)-lipgloss.Width(separator))
	meta := version
	if pathWidth > 0 {
		meta += separator + truncateMiddleColumns(workspace, pathWidth)
	}
	if m.colorAnimationEnabled() {
		return renderThemeGradient(art, m.colorAnimationElapsed, true) + "\n\n" +
			renderThemeGradient(meta, m.colorAnimationElapsed, false)
	}
	return m.styles.Accent.Bold(true).Render(art) + "\n\n" + m.styles.Muted.Render(meta)
}

func renderThemeGradient(value string, elapsed time.Duration, bold bool) string {
	if value == "" {
		return ""
	}
	var output strings.Builder
	output.Grow(len(value) * 20)
	column := 0
	for _, character := range value {
		if character == '\n' {
			output.WriteString("\x1b[0m\n")
			column = 0
			continue
		}
		rgb := themeGradientRGB(column, elapsed)
		if bold {
			fmt.Fprintf(&output, "\x1b[1;38;2;%d;%d;%dm%c", rgb[0], rgb[1], rgb[2], character)
		} else {
			fmt.Fprintf(&output, "\x1b[38;2;%d;%d;%dm%c", rgb[0], rgb[1], rgb[2], character)
		}
		column += ansi.StringWidth(string(character))
	}
	output.WriteString("\x1b[0m")
	return output.String()
}

func themeGradientRGB(column int, elapsed time.Duration) [3]uint8 {
	phase := float64(column)*colorSpatialFrequency + elapsed.Seconds()*colorTemporalFrequency
	brightness := 0.35 + 0.65*(math.Sin(phase)+1)/2
	channel := func(accent uint8) uint8 {
		return uint8(math.Round(float64(accent) * brightness))
	}
	return [3]uint8{channel(eyluAccentRGB[0]), channel(eyluAccentRGB[1]), channel(eyluAccentRGB[2])}
}

func (m *Model) renderTimelineMarkdown(item *timelineItem) string {
	if item == nil {
		return ""
	}
	contentWidth := m.viewportContentWidth()
	if item.renderedSource == item.text && item.renderedWidth == contentWidth &&
		item.renderedWorkspace == m.snapshot.Workspace && item.renderedNoColor == m.noColor {
		return item.renderedText
	}
	item.renderedSource = item.text
	item.renderedWidth = contentWidth
	item.renderedWorkspace = m.snapshot.Workspace
	item.renderedNoColor = m.noColor
	item.renderedText = m.renderMarkdown(item.text)
	return item.renderedText
}

func (m *Model) renderTool(tool *toolView) string {
	if tool == nil {
		return ""
	}
	state := "done"
	if tool.preparing {
		state = "generating"
	} else if tool.running && (tool.name == "write_file" || tool.name == "edit_file") {
		state = "applying"
	} else if tool.running {
		state = "running"
	} else if tool.isError {
		state = "failed"
	} else if tool.interrupted {
		state = "interrupted"
	}
	duration := ""
	if tool.durationMS > 0 {
		duration = "  " + FormatDurationMS(tool.durationMS)
	}
	contentWidth := m.viewportContentWidth()
	detail := summarizeLine(tool.arguments, max(20, contentWidth-30))
	if tool.path != "" {
		detail = m.renderToolFileDetail(tool)
	}
	lines := []string{fmt.Sprintf("> %s  %s%s", tool.name, state, duration)}
	if detail != "" {
		lines = append(lines, "  "+ansi.Truncate(detail, max(10, contentWidth-2), "..."))
	}
	if tool.preview != "" {
		for _, line := range strings.Split(tool.preview, "\n") {
			lines = append(lines, "  "+truncateColumns(line, max(10, contentWidth-2)))
		}
	}
	return m.styles.Tool.Render(strings.Join(lines, "\n"))
}

func (m *Model) renderToolFileDetail(tool *toolView) string {
	location := m.renderFileLocationLink(tool.path)
	if !tool.fileStatsKnown {
		return location
	}
	lines := fmt.Sprintf("%d lines", tool.generatedLines)
	if !tool.linesComplete {
		lines = fmt.Sprintf("%d+ lines", tool.generatedLines)
	}
	return fmt.Sprintf("%s  %s  %s", location, formatByteCount(tool.generatedBytes), lines)
}

func (m *Model) taskPanelRows() int {
	if m.screen != screenChat || !m.busy() || m.approval != nil || m.ask != nil || m.planGate != nil || len(m.snapshot.TodoList.Items) == 0 {
		return 0
	}
	desired := min(len(m.snapshot.TodoList.Items), maxTaskPanelItems)
	if len(m.snapshot.TodoList.Items) > maxTaskPanelItems {
		desired++
	}
	available := max(0, m.height-m.input.Height()-fixedChromeRows-minViewportRows)
	return min(desired, available)
}

func (m *Model) inlineTaskPanelVisible() bool {
	return m.screen == screenChat && !m.busy() && m.approval == nil && m.ask == nil && m.planGate == nil && len(m.snapshot.TodoList.Items) > 0
}

func (m *Model) renderInlineTaskPanel() string {
	items := orderedTodoItems(m.snapshot.TodoList.Items)
	desired := 1 + min(len(items), maxTaskPanelItems)
	if len(items) > maxTaskPanelItems {
		desired++
	}
	available := max(minViewportRows, m.height-m.input.Height()-fixedChromeRows-m.completionHeight())
	rows := min(desired, max(1, available))
	lines := []string{m.styles.Status.Render(todoSummaryLabel(items))}
	contentRows := rows - 1
	if contentRows <= 0 {
		return lines[0]
	}
	visible := min(len(items), min(maxTaskPanelItems, contentRows))
	showOverflow := visible < len(items)
	if showOverflow && contentRows > 1 {
		visible = min(visible, contentRows-1)
	}
	for index := 0; index < visible; index++ {
		lines = append(lines, m.renderTaskPanelItem(items[index], "  ", m.viewportContentWidth()))
	}
	if showOverflow && len(lines) < rows {
		lines = append(lines, m.styles.Muted.Render(truncateColumns("  ... "+todoOverflowLabel(items[visible:]), m.viewportContentWidth())))
	}
	return strings.Join(lines, "\n")
}

func todoSummaryLabel(items []protocol.TodoItem) string {
	done, inProgress, open, cancelled := 0, 0, 0, 0
	for _, item := range items {
		switch item.Status {
		case protocol.TodoCompleted:
			done++
		case protocol.TodoInProgress:
			inProgress++
		case protocol.TodoPending:
			open++
		case protocol.TodoCancelled:
			cancelled++
		}
	}
	label := "tasks"
	if len(items) == 1 {
		label = "task"
	}
	summary := fmt.Sprintf("%d %s (%d done, %d in progress, %d open", len(items), label, done, inProgress, open)
	if cancelled > 0 {
		summary += fmt.Sprintf(", %d cancelled", cancelled)
	}
	return summary + ")"
}

func (m *Model) renderTaskPanel(rows int) string {
	if rows <= 0 {
		return ""
	}
	contentWidth := m.viewportContentWidth()
	items := orderedTodoItems(m.snapshot.TodoList.Items)
	visible := min(len(items), min(maxTaskPanelItems, rows))
	showOverflow := visible < len(items)
	if showOverflow && rows > 1 {
		visible = min(visible, rows-1)
	}
	lines := make([]string, 0, rows)
	for index := 0; index < visible; index++ {
		prefix := "  "
		if index == 0 {
			prefix = "└ "
		}
		lines = append(lines, m.renderTaskPanelItem(items[index], prefix, contentWidth))
	}
	if showOverflow && len(lines) < rows {
		lines = append(lines, m.styles.Muted.Render(truncateColumns("  ... "+todoOverflowLabel(items[visible:]), contentWidth)))
	}
	return indentBlock(strings.Join(lines, "\n"), m.viewportLeftInset())
}

func (m *Model) renderTaskPanelItem(item protocol.TodoItem, prefix string, width int) string {
	marker := "[ ]"
	style := m.styles.Agent
	switch item.Status {
	case protocol.TodoInProgress:
		marker = "[>]"
		style = m.styles.Accent
	case protocol.TodoCompleted:
		marker = "[x]"
		style = m.styles.Active
	case protocol.TodoCancelled:
		marker = "[-]"
		style = m.styles.Muted
	}
	plainPrefix := prefix + marker + " "
	content := truncateColumns(item.Content, max(1, width-lipgloss.Width(plainPrefix)))
	return m.styles.Muted.Render(prefix) + style.Render(marker+" "+content)
}

func orderedTodoItems(items []protocol.TodoItem) []protocol.TodoItem {
	ordered := make([]protocol.TodoItem, 0, len(items))
	for _, status := range []protocol.TodoStatus{protocol.TodoInProgress, protocol.TodoPending, protocol.TodoCancelled, protocol.TodoCompleted} {
		for _, item := range items {
			if item.Status == status {
				ordered = append(ordered, item)
			}
		}
	}
	return ordered
}

func todoOverflowLabel(items []protocol.TodoItem) string {
	pending, completed, cancelled := 0, 0, 0
	for _, item := range items {
		switch item.Status {
		case protocol.TodoPending, protocol.TodoInProgress:
			pending++
		case protocol.TodoCompleted:
			completed++
		case protocol.TodoCancelled:
			cancelled++
		}
	}
	parts := make([]string, 0, 3)
	if pending > 0 {
		parts = append(parts, fmt.Sprintf("+%d pending", pending))
	}
	if completed > 0 {
		parts = append(parts, fmt.Sprintf("+%d completed", completed))
	}
	if cancelled > 0 {
		parts = append(parts, fmt.Sprintf("+%d cancelled", cancelled))
	}
	return strings.Join(parts, "  ")
}

func todoProgress(list protocol.TodoList) (int, int, *protocol.TodoItem, *protocol.TodoItem) {
	completed, total := 0, 0
	currentIndex := -1
	for index := range list.Items {
		item := &list.Items[index]
		if item.Status != protocol.TodoCancelled {
			total++
		}
		if item.Status == protocol.TodoCompleted {
			completed++
		}
		if currentIndex < 0 && item.Status == protocol.TodoInProgress {
			currentIndex = index
		}
	}
	if currentIndex < 0 {
		for index := range list.Items {
			if list.Items[index].Status == protocol.TodoPending {
				currentIndex = index
				break
			}
		}
	}
	var current, next *protocol.TodoItem
	if currentIndex >= 0 {
		current = &list.Items[currentIndex]
	}
	for index := range list.Items {
		if index != currentIndex && list.Items[index].Status == protocol.TodoPending {
			next = &list.Items[index]
			break
		}
	}
	return completed, total, current, next
}

func FormatDurationMS(milliseconds int64) string {
	if milliseconds < 0 {
		milliseconds = 0
	}
	duration := time.Duration(milliseconds) * time.Millisecond
	if duration < time.Second {
		return fmt.Sprintf("%dms", milliseconds)
	}
	return duration.String()
}

func (m *Model) renderFileLocationLink(path string) string {
	display := m.workspaceRelativeDisplayPath(path)
	if m.noColor || path == "" {
		return display
	}
	directoryURL, ok := localContainingDirectoryURL(m.snapshot.Workspace, path)
	if !ok {
		return display
	}
	return ansi.SetHyperlink(directoryURL) + display + ansi.ResetHyperlink()
}

func (m *Model) workspaceRelativeDisplayPath(path string) string {
	if path == "" || m.snapshot.Workspace == "" || !filepath.IsAbs(path) {
		return path
	}
	relative, err := filepath.Rel(m.snapshot.Workspace, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return path
	}
	return filepath.ToSlash(relative)
}

func (m *Model) renderToolDetail() string {
	if m.toolCursor < 0 || m.toolCursor >= len(m.timeline) || m.timeline[m.toolCursor].tool == nil {
		return ""
	}
	tool := m.timeline[m.toolCursor].tool
	preview := tool.preview
	if preview == "" {
		preview = "(no file preview)"
	}
	return fmt.Sprintf("%s\n\nLive change\n%s\n\nArguments\n%s\n\nResult\n%s", m.styles.Tool.Render(tool.name), preview, tool.arguments, tool.content)
}

func (m *Model) renderProviders() string {
	var output strings.Builder
	output.WriteString(m.styles.Header.Render("Providers") + "\n\n")
	for index, item := range m.snapshot.Providers {
		cursor := "  "
		if index == m.providerCursor {
			cursor = "> "
		}
		active := " "
		if item.Active {
			active = "*"
		}
		line := fmt.Sprintf("%s%s %-18s %-20s %s", cursor, active, item.Name, item.Adapter, item.Model)
		line = truncateColumns(line, m.viewportContentWidth())
		if item.Active {
			line = m.styles.Active.Render(line)
		}
		output.WriteString(line + "\n")
	}
	return output.String()
}

func (m *Model) renderModels() string {
	var output strings.Builder
	output.WriteString(m.styles.Header.Render("Models") + "\n")
	output.WriteString(m.modelFilter.View() + "\n\n")
	if m.state == StateFetchingModels {
		output.WriteString(m.styles.Loading.Render("Fetching models...") + "\n")
		return output.String()
	}
	filtered := m.filteredModels()
	page := m.modelPage(len(filtered))
	if len(filtered) == 0 {
		output.WriteString(m.styles.Muted.Render("No models found.") + "\n")
	}
	for index := page.start; index < page.end; index++ {
		model := filtered[index]
		cursor := "  "
		if index == m.modelCursor {
			cursor = "> "
		}
		line := cursor + truncateColumns(model, max(8, m.viewportContentWidth()-3))
		if model == m.snapshot.Model {
			line = m.styles.Active.Render(line)
		}
		output.WriteString(line + "\n")
	}
	output.WriteString(m.renderModelsFooter(page))
	return output.String()
}

func (m *Model) renderModelsFooter(page modelPageState) string {
	full := fmt.Sprintf("Page %d/%d · ←/→ page · ↑/↓ select · Enter use · m manual · r refresh · Esc back", page.number, page.total)
	compact := fmt.Sprintf("%d/%d · ←/→ page · ↑/↓ select · Enter", page.number, page.total)
	if m.modelManual {
		full = fmt.Sprintf("Page %d/%d · Manual ID · Enter use · Esc back", page.number, page.total)
		compact = fmt.Sprintf("%d/%d · Manual ID · Enter · Esc", page.number, page.total)
	}
	footer := full
	width := m.viewportContentWidth()
	if lipgloss.Width(footer) > width {
		footer = compact
	}
	return m.styles.Muted.Render(truncateColumns(footer, width))
}

func (m *Model) renderContextWindowConfirmation() string {
	state := m.contextWindowConfirm
	if state == nil {
		return ""
	}
	selection := state.selection
	var output strings.Builder
	output.WriteString(m.styles.Header.Render("Confirm context window") + "\n\n")
	fmt.Fprintf(&output, "Model     %s\nDetected  %d tokens\nSource    %s", selection.Model, selection.DetectedContextWindow, selection.LimitSource)
	if selection.Cached {
		output.WriteString(" · cached")
	}
	if selection.Assumed {
		output.WriteString(" · assumed")
	}
	output.WriteString("\n\nIs this context window correct?\n\n")
	choices := []string{"Use detected value", "Enter a different value"}
	for index, choice := range choices {
		prefix := "  "
		if index == state.cursor {
			prefix = "> "
			choice = m.styles.Active.Render(choice)
		}
		output.WriteString(prefix + choice + "\n")
	}
	if state.editing {
		output.WriteString("\n" + state.input.View() + "\n")
	}
	if state.err != "" {
		output.WriteString("\n" + m.styles.Error.Render(state.err) + "\n")
	}
	return output.String()
}

func (m *Model) renderSkills() string {
	var output strings.Builder
	output.WriteString(m.styles.Header.Render("Skills") + "\n\n")
	for index, item := range m.snapshot.Skills {
		cursor := "  "
		if index == m.skillCursor {
			cursor = "> "
		}
		active := " "
		if item.Activated {
			active = "*"
		}
		line := fmt.Sprintf("%s%s %-24s %-16s %s", cursor, active, item.Name, item.Source, item.Status)
		line = truncateColumns(line, m.viewportContentWidth())
		if item.Activated {
			line = m.styles.Active.Render(line)
		}
		output.WriteString(line + "\n")
		if index == m.skillCursor && item.Reason != "" {
			output.WriteString(m.styles.Muted.Render("    "+item.Reason) + "\n")
		}
		if index == m.skillCursor && item.ShadowedBy != "" {
			output.WriteString(m.styles.Muted.Render("    shadowed by "+item.ShadowedBy) + "\n")
		}
	}
	return output.String()
}

func (m *Model) renderMCPServers() string {
	var output strings.Builder
	width := m.viewportContentWidth()
	output.WriteString(m.styles.Header.Render("MCP servers") + "\n\n")
	if m.mcpLoading && len(m.mcpServers) == 0 {
		prefix := "*"
		if m.animation {
			prefix = m.mcpSpinner.View()
		}
		output.WriteString(m.styles.Loading.Render(prefix + " Loading MCP runtime..."))
		return output.String()
	}
	if len(m.mcpServers) == 0 {
		output.WriteString(m.styles.Muted.Render("No configured MCP servers."))
	} else {
		for index, item := range m.mcpServers {
			cursor := "  "
			if index == m.mcpCursor {
				cursor = "> "
			}
			protocolVersion := item.ProtocolVersion
			if protocolVersion == "" {
				protocolVersion = "-"
			}
			line := fmt.Sprintf("%s%-20s %-13s %-17s %-10s T:%d R:%d P:%d", cursor, item.Name, item.Status, item.Transport, protocolVersion, item.ToolCount, item.ResourceCount, item.PromptCount)
			line = truncateColumns(line, width)
			output.WriteString(m.renderMCPStatus(item.Status, line) + "\n")
			if item.LastError != "" {
				output.WriteString(m.styles.Error.Render(truncateColumns("    "+item.LastError, width)) + "\n")
			}
		}
	}
	if m.mcpNotice != "" {
		style := m.styles.Status
		if m.mcpNoticeError {
			style = m.styles.Error
		}
		output.WriteString("\n" + style.Render(wrapPlain(m.mcpNotice, width)) + "\n")
	}
	output.WriteString("\n" + m.styles.Muted.Render("Enter details · r reconnect · e enable · d disable · l login · o logout · g refresh · Esc back"))
	return strings.TrimRight(output.String(), "\n")
}

func (m *Model) renderMCPStatus(status, value string) string {
	switch status {
	case "connected":
		return m.styles.Active.Render(value)
	case "connecting", "reconnecting":
		return m.styles.Loading.Render(value)
	case "needs_auth":
		return m.styles.Header.Render(value)
	case "error":
		return m.styles.Error.Render(value)
	default:
		return m.styles.Muted.Render(value)
	}
}

func (m *Model) renderMCPDetail() string {
	server, ok := m.selectedMCPServer()
	if !ok {
		return m.styles.Muted.Render("MCP server is no longer configured.\n\nEsc back")
	}
	width := m.viewportContentWidth()
	var output strings.Builder
	title := server.Name + "  " + server.Status
	output.WriteString(m.styles.Header.Render(truncateColumns(title, width)) + "\n")
	tabs := []string{"1 Details", "2 Tools", "3 Resources", "4 Prompts"}
	for index := range tabs {
		if index == m.mcpTab {
			tabs[index] = m.styles.Active.Bold(true).Render(tabs[index])
		} else {
			tabs[index] = m.styles.Muted.Render(tabs[index])
		}
	}
	output.WriteString(strings.Join(tabs, "  ") + "\n\n")
	switch m.mcpTab {
	case 1:
		m.renderMCPTools(&output, server, width)
	case 2:
		m.renderMCPResources(&output, server, width)
	case 3:
		m.renderMCPPrompts(&output, server, width)
	default:
		m.renderMCPServerDetails(&output, server, width)
	}
	if m.mcpNotice != "" {
		style := m.styles.Status
		if m.mcpNoticeError {
			style = m.styles.Error
		}
		output.WriteString("\n" + style.Render(wrapPlain(m.mcpNotice, width)) + "\n")
	}
	footer := "←/→ or 1-4 view · r reconnect · e enable · d disable · l login · o logout · g refresh · Esc servers"
	if m.mcpTab == 1 && len(server.Tools) > 0 {
		footer = "Enter tool details · " + footer
	}
	output.WriteString("\n" + m.styles.Muted.Render(footer))
	return strings.TrimRight(output.String(), "\n")
}

func (m *Model) renderMCPServerDetails(output *strings.Builder, server MCPServerItem, width int) {
	implementation := strings.Trim(strings.Join([]string{server.Implementation, server.Version}, " "), " ")
	m.renderMCPField(output, "Transport", server.Transport, width)
	m.renderMCPField(output, "Protocol", server.ProtocolVersion, width)
	m.renderMCPField(output, "Implementation", implementation, width)
	if server.ConnectDurationMS > 0 {
		m.renderMCPField(output, "Connected in", fmt.Sprintf("%dms", server.ConnectDurationMS), width)
	}
	m.renderMCPField(output, "Catalog", fmt.Sprintf("%d tools · %d resources · %d prompts", server.ToolCount, server.ResourceCount, server.PromptCount), width)
	m.renderMCPField(output, "Last error", server.LastError, width)
	m.renderMCPField(output, "Capabilities", server.Capabilities, width)
	m.renderMCPField(output, "Instructions", server.Instructions, width)
}

func (m *Model) renderMCPTools(output *strings.Builder, server MCPServerItem, width int) {
	if len(server.Tools) == 0 {
		output.WriteString(m.styles.Muted.Render("No tools reported."))
		return
	}
	for index, item := range server.Tools {
		prefix := "  "
		if index == m.mcpCatalogCursor {
			prefix = "> "
		}
		line := truncateColumns(prefix+item.Name+"  "+item.Status+"  "+item.Permission, width)
		output.WriteString(line + "\n")
	}
}

func (m *Model) renderMCPToolDetail() string {
	server, item, ok := m.selectedMCPTool()
	if !ok {
		return m.styles.Muted.Render("MCP tool is no longer available.\n\nEsc tools")
	}
	width := m.viewportContentWidth()
	var output strings.Builder
	output.WriteString(m.styles.Header.Render(truncateColumns(server.Name+" / Tool", width)) + "\n\n")
	output.WriteString(m.styles.Active.Bold(true).Render(truncateColumns(item.Name+"  "+item.Status+"  "+item.Permission, width)) + "\n\n")
	m.renderMCPField(&output, "Local name", item.LocalName, width)
	m.renderMCPField(&output, "Description", item.Description, width)
	m.renderMCPField(&output, "Annotations", item.Annotations, width)
	m.renderMCPField(&output, "Input schema", item.InputSchema, width)
	m.renderMCPField(&output, "Output schema", item.OutputSchema, width)
	output.WriteString("\n" + m.styles.Muted.Render("↑/↓ scroll · g refresh · Esc tools"))
	return strings.TrimRight(output.String(), "\n")
}

func (m *Model) renderMCPResources(output *strings.Builder, server MCPServerItem, width int) {
	if len(server.Resources) == 0 {
		output.WriteString(m.styles.Muted.Render("No resources reported."))
		return
	}
	for index, item := range server.Resources {
		prefix := "  "
		if index == m.mcpCatalogCursor {
			prefix = "> "
		}
		output.WriteString(truncateColumns(prefix+item.Name+"  "+item.URI, width) + "\n")
		if index == m.mcpCatalogCursor {
			m.renderMCPField(output, "MIME type", item.MIMEType, width)
			m.renderMCPField(output, "Description", item.Description, width)
		}
	}
}

func (m *Model) renderMCPPrompts(output *strings.Builder, server MCPServerItem, width int) {
	if len(server.Prompts) == 0 {
		output.WriteString(m.styles.Muted.Render("No prompts reported."))
		return
	}
	for index, item := range server.Prompts {
		prefix := "  "
		if index == m.mcpCatalogCursor {
			prefix = "> "
		}
		output.WriteString(truncateColumns(prefix+item.Name, width) + "\n")
		if index == m.mcpCatalogCursor {
			m.renderMCPField(output, "Description", item.Description, width)
			m.renderMCPField(output, "Arguments", item.Arguments, width)
		}
	}
}

func (m *Model) renderMCPField(output *strings.Builder, label, value string, width int) {
	if strings.TrimSpace(value) == "" {
		return
	}
	prefix := label + "  "
	available := max(1, width-lipgloss.Width(prefix))
	lines := strings.Split(wrapPlain(value, available), "\n")
	for index, line := range lines {
		if index == 0 {
			output.WriteString(m.styles.Muted.Render(prefix) + line + "\n")
		} else {
			output.WriteString(strings.Repeat(" ", lipgloss.Width(prefix)) + line + "\n")
		}
	}
}

func (m *Model) renderContext() string {
	report := m.displayContextReport()
	var output strings.Builder
	width := m.viewportContentWidth()
	provider := report.Provider
	if provider == "" {
		provider = m.snapshot.Provider
	}
	model := report.Model
	if model == "" {
		model = m.snapshot.Model
	}
	identity := strings.Trim(strings.Join([]string{provider, model}, " · "), " ·")
	fmt.Fprintf(&output, "%s\n", m.styles.Header.Render("Context map"))
	if identity != "" {
		fmt.Fprintf(&output, "%s\n", m.styles.Status.Render(truncateColumns(identity, width)))
	}
	if report.LimitKnown && report.ContextWindow > 0 {
		used := roundedContextPercent(report)
		barWidth := min(32, max(12, width-13))
		fmt.Fprintf(&output, "\n%s  %d%% used\n", m.renderContextSignal(report, barWidth), used)
		free := max(0, report.ContextWindow-report.TotalTokens)
		fmt.Fprintf(&output, "%s input · %s reserve · %s free\n",
			formatTokenCount(report.InputTokens), formatTokenCount(report.OutputReserve), formatTokenCount(free))
	} else {
		fmt.Fprintf(&output, "\n%s\n", m.styles.Muted.Render("Limit unknown · "+formatTokenCount(report.TotalTokens)+" tokens tracked"))
		fmt.Fprintf(&output, "%s input · %s reserve\n", formatTokenCount(report.InputTokens), formatTokenCount(report.OutputReserve))
	}
	fmt.Fprintf(&output, "%s\n", m.styles.Muted.Render("◆ input  ◇ reserve  · free"))

	fmt.Fprintf(&output, "\n%s\n", m.styles.Header.Render("Footprint"))
	for _, group := range groupedContextUsage(report) {
		output.WriteString(m.renderContextUsageRow("◆", group.name, group.tokens, report))
		output.WriteByte('\n')
	}
	if report.OutputReserve > 0 {
		output.WriteString(m.renderContextUsageRow("◇", "Output reserve", report.OutputReserve, report))
		output.WriteByte('\n')
	}
	if report.LimitKnown && report.ContextWindow > 0 {
		free := max(0, report.ContextWindow-report.TotalTokens)
		output.WriteString(m.renderContextUsageRow("·", "Free", free, report))
		output.WriteByte('\n')
	}

	if m.contextExpand {
		m.renderContextDetails(&output, report, width)
	}
	footer := "Enter show details · Esc back"
	if m.contextExpand {
		footer = "Enter hide details · Esc back"
	}
	fmt.Fprintf(&output, "\n%s", m.styles.Muted.Render(footer))
	return strings.TrimRight(output.String(), "\n")
}

func (m *Model) displayContextReport() contextledger.Report {
	report := m.snapshot.Context
	if m.contextStarted {
		return report
	}
	report.InputTokens = 0
	report.OutputReserve = 0
	report.TotalTokens = 0
	report.Percent = 0
	report.Categories = nil
	report.LastUsage = protocol.Usage{}
	report.CompressionCount = 0
	report.LastCompression = nil
	return report
}

type contextUsageGroup struct {
	name   string
	tokens int
}

func groupedContextUsage(report contextledger.Report) []contextUsageGroup {
	order := []string{"System", "Conversation", "Tools", "Skills", "MCP", "Model state", "Other"}
	totals := make(map[string]int, len(order))
	for _, category := range report.Categories {
		if category.Tokens <= 0 || category.Category == contextledger.CategoryOutputReserve {
			continue
		}
		totals[contextUsageGroupName(category.Category)] += category.Tokens
	}
	groups := make([]contextUsageGroup, 0, len(order))
	for _, name := range order {
		if totals[name] > 0 {
			groups = append(groups, contextUsageGroup{name: name, tokens: totals[name]})
		}
	}
	return groups
}

func contextUsageGroupName(category contextledger.Category) string {
	switch category {
	case contextledger.CategorySystemPrompt, contextledger.CategoryTaskState, contextledger.CategoryProjectContext:
		return "System"
	case contextledger.CategoryUserMessage, contextledger.CategoryAgentMessage, contextledger.CategorySummary:
		return "Conversation"
	case contextledger.CategoryBuiltinToolSchema, contextledger.CategoryBuiltinToolResult:
		return "Tools"
	case contextledger.CategorySkillCatalog, contextledger.CategorySkillBody, contextledger.CategorySkillResource:
		return "Skills"
	case contextledger.CategoryMCPInstructions, contextledger.CategoryMCPToolSchema, contextledger.CategoryMCPResource, contextledger.CategoryMCPToolResult:
		return "MCP"
	case contextledger.CategoryDriverState:
		return "Model state"
	default:
		return "Other"
	}
}

func (m *Model) renderContextUsageRow(marker, label string, tokens int, report contextledger.Report) string {
	markerStyle := m.styles.Accent
	if marker == "◇" {
		markerStyle = m.styles.Tool
	} else if marker == "·" {
		markerStyle = m.styles.Muted
	}
	value := fmt.Sprintf("%-15s %8s", truncateColumns(label, 15), formatTokenCount(tokens))
	if report.LimitKnown && report.ContextWindow > 0 {
		percent := int(math.Round(float64(tokens) / float64(report.ContextWindow) * 100))
		value += fmt.Sprintf(" %4d%%", max(0, percent))
	}
	return markerStyle.Render(marker) + " " + value
}

func (m *Model) renderContextSignal(report contextledger.Report, width int) string {
	width = max(1, width)
	denominator := max(report.ContextWindow, report.TotalTokens)
	if denominator <= 0 {
		return m.styles.Muted.Render(strings.Repeat("·", width))
	}
	inputCells := int(math.Round(float64(report.InputTokens) / float64(denominator) * float64(width)))
	reserveCells := int(math.Round(float64(report.OutputReserve) / float64(denominator) * float64(width)))
	if report.InputTokens > 0 && inputCells == 0 {
		inputCells = 1
	}
	if report.OutputReserve > 0 && reserveCells == 0 {
		reserveCells = 1
	}
	if inputCells+reserveCells > width {
		reserveCells = max(0, width-inputCells)
	}
	if report.TotalTokens >= report.ContextWindow {
		reserveCells = max(0, width-inputCells)
	}
	freeCells := max(0, width-inputCells-reserveCells)
	return m.styles.Accent.Render(strings.Repeat("◆", min(width, inputCells))) +
		m.styles.Tool.Render(strings.Repeat("◇", min(max(0, width-inputCells), reserveCells))) +
		m.styles.Muted.Render(strings.Repeat("·", freeCells))
}

func (m *Model) renderContextDetails(output *strings.Builder, report contextledger.Report, width int) {
	fmt.Fprintf(output, "\n%s\n", m.styles.Header.Render("Details"))
	limitLine := fmt.Sprintf("Configured %s · detected %s · effective %s",
		formatOptionalTokenCount(report.ConfiguredContextWindow), formatOptionalTokenCount(report.DetectedContextWindow), formatOptionalTokenCount(report.ContextWindow))
	output.WriteString(wrapPlain(limitLine, width) + "\n")
	source := report.LimitSource
	if source == "" {
		source = "unknown"
	}
	flags := []string{"source " + source}
	if report.LimitCached {
		flags = append(flags, "cached")
	}
	if report.LimitAssumed {
		flags = append(flags, "assumed")
	}
	if report.LimitDegradations > 0 {
		flags = append(flags, fmt.Sprintf("%d degradations", report.LimitDegradations))
	}
	output.WriteString(m.styles.Muted.Render(wrapPlain(strings.Join(flags, " · "), width)) + "\n")

	fmt.Fprintf(output, "\n%s\n", m.styles.Header.Render("Categories"))
	for _, category := range report.Categories {
		if category.Tokens <= 0 {
			continue
		}
		line := fmt.Sprintf("%-18s %8s", truncateColumns(category.Label, 18), formatTokenCount(category.Tokens))
		if report.LimitKnown && report.ContextWindow > 0 {
			percent := int(math.Round(float64(category.Tokens) / float64(report.ContextWindow) * 100))
			line += fmt.Sprintf(" %4d%%", max(0, percent))
		}
		if width >= 48 && category.Measurement != "" {
			line += "  " + category.Measurement
		}
		output.WriteString(truncateColumns(line, width) + "\n")
		for _, source := range category.Sources {
			if source.Tokens <= 0 {
				continue
			}
			sourceLine := fmt.Sprintf("  %-16s %8s", truncateColumns(source.Source, 16), formatTokenCount(source.Tokens))
			if report.LimitKnown && report.ContextWindow > 0 {
				percent := int(math.Round(float64(source.Tokens) / float64(report.ContextWindow) * 100))
				sourceLine += fmt.Sprintf(" %4d%%", max(0, percent))
			}
			output.WriteString(m.styles.Muted.Render(truncateColumns(sourceLine, width)) + "\n")
		}
	}
	if report.CompressionCount > 0 {
		fmt.Fprintf(output, "\n%s\n", m.styles.Header.Render("Compression"))
		fmt.Fprintf(output, "%d compactions\n", report.CompressionCount)
		if report.LastCompression != nil {
			line := fmt.Sprintf("Last %s → %s · %d turns summarized",
				formatTokenCount(report.LastCompression.BeforeTokens), formatTokenCount(report.LastCompression.AfterTokens), report.LastCompression.OmittedTurns)
			output.WriteString(m.styles.Muted.Render(wrapPlain(line, width)) + "\n")
		}
	}
	if report.LastUsage.InputTokens > 0 || report.LastUsage.OutputTokens > 0 {
		fmt.Fprintf(output, "\n%s\n", m.styles.Header.Render("Provider usage"))
		fmt.Fprintf(output, "%s input · %s output\n", formatTokenCount(report.LastUsage.InputTokens), formatTokenCount(report.LastUsage.OutputTokens))
	}
}

func (m *Model) renderTasks() string {
	var output strings.Builder
	completed, total, _, _ := todoProgress(m.snapshot.TodoList)
	fmt.Fprintf(&output, "%s\n%d/%d complete\n\n", m.styles.Header.Render("Tasks"), completed, total)
	if len(m.snapshot.TodoList.Items) == 0 {
		output.WriteString(m.styles.Muted.Render("No tasks."))
		return output.String()
	}
	for _, item := range orderedTodoItems(m.snapshot.TodoList.Items) {
		marker := "[ ]"
		switch item.Status {
		case protocol.TodoInProgress:
			marker = "[>]"
		case protocol.TodoCompleted:
			marker = "[x]"
		case protocol.TodoCancelled:
			marker = "[-]"
		}
		fmt.Fprintf(&output, "%s %s\n", marker, wrapLimited(item.Content, max(8, m.viewportContentWidth()-4), 2))
	}
	return output.String()
}

func (m *Model) renderAsk(height int) string {
	if m.ask == nil || m.ask.request == nil || len(m.ask.request.Questions) == 0 {
		return ""
	}
	question := m.ask.request.Questions[m.ask.question]
	contentWidth := max(12, m.width-4)
	meta := fmt.Sprintf("%d/%d", m.ask.question+1, len(m.ask.request.Questions))
	if m.ask.err != "" {
		meta = m.ask.err
	}
	lines := []string{panelHeader(m.styles.Accent.Bold(true).Render(question.Header), meta, contentWidth, m.styles)}
	if height <= 7 {
		lines = append(lines, m.styles.Agent.Render(truncateColumns(question.Question, contentWidth)))
		label, description := "Other", "Type a custom answer"
		if m.ask.cursor < len(question.Options) {
			label = question.Options[m.ask.cursor].Label
			description = question.Options[m.ask.cursor].Description
		}
		lines = append(lines, m.styles.Active.Render("> "+label)+"  "+m.styles.Muted.Render(truncateColumns(description, max(8, contentWidth-lipgloss.Width(label)-4))))
	} else {
		lines = append(lines, m.styles.Agent.Render(wrapLimited(question.Question, contentWidth, 2)))
		selected := m.ask.selections[question.ID]
		for index, option := range question.Options {
			marker := "( )"
			if question.Multiple {
				marker = "[ ]"
			}
			if selected[index] {
				if question.Multiple {
					marker = "[x]"
				} else {
					marker = "(*)"
				}
			}
			cursor := "  "
			if index == m.ask.cursor {
				cursor = "> "
			}
			line := cursor + marker + " " + option.Label + "  " + m.styles.Muted.Render(option.Description)
			lines = append(lines, truncateColumns(line, contentWidth))
		}
		otherMarker := "( )"
		if question.Multiple {
			otherMarker = "[ ]"
		}
		if strings.TrimSpace(m.ask.custom[question.ID]) != "" {
			if question.Multiple {
				otherMarker = "[x]"
			} else {
				otherMarker = "(*)"
			}
		}
		cursor := "  "
		if m.ask.cursor == len(question.Options) {
			cursor = "> "
		}
		lines = append(lines, cursor+otherMarker+" Other  "+m.styles.Muted.Render("Type a custom answer"))
	}
	if m.ask.editing {
		lines = append(lines, m.styles.Muted.Render("Custom answer"), m.ask.input.View())
	}
	footer := "↑/↓ select  ·  Space toggle  ·  Enter submit  ·  Tab custom  ·  ← previous  ·  Esc cancel"
	if m.height < 18 {
		footer = "↑/↓ select  ·  Enter submit  ·  Tab custom  ·  Esc cancel"
	}
	return m.renderBottomPanel(lines, footer, height)
}

func (m *Model) renderApproval(height int) string {
	approval := m.approval
	if approval == nil {
		return ""
	}
	accent := m.styles.Warning
	if approval.Warning {
		accent = m.styles.Error
	}
	contentWidth := max(12, m.width-4)
	header := panelHeader(accent.Bold(true).Render("Action approval"), fmt.Sprintf("%d/%d  %s", approval.Step, approval.Total, strings.ToUpper(approval.Risk)), contentWidth, m.styles)
	lines := []string{header}
	if height <= 7 {
		lines = append(lines,
			m.styles.Tool.Bold(true).Render(approval.Tool)+"  "+truncateColumns(approval.Summary, max(8, contentWidth-lipgloss.Width(approval.Tool)-2)),
			m.styles.Muted.Render("Why  ")+truncateColumns(approval.Reason, max(8, contentWidth-5)),
			renderChoiceRow([]string{"Yes", "No"}, m.approvalCursor, contentWidth, m.styles),
		)
	} else {
		lines = append(lines,
			m.styles.Tool.Bold(true).Render(approval.Tool),
			m.styles.Agent.Render(wrapLimited(approval.Summary, contentWidth, 1)),
			m.styles.Muted.Render("Why  ")+wrapLimited(approval.Reason, max(8, contentWidth-5), 1),
			m.styles.Muted.Render("Policy  "+truncateColumns(approval.PolicyReason, max(8, contentWidth-8))),
			renderChoiceRow([]string{"Yes, run once", "No, reject"}, m.approvalCursor, contentWidth, m.styles),
		)
	}
	if m.approvalEditing || strings.TrimSpace(m.approvalReason.Value()) != "" {
		lines = append(lines, "", m.styles.Muted.Render("Rejection feedback"), m.approvalReason.View())
	}
	footer := "↑/↓ select  ·  Enter confirm  ·  Tab add rejection reason  ·  Esc reject"
	if m.height < 18 {
		footer = "Enter confirm  ·  Tab reason  ·  Esc reject"
	}
	return m.renderBottomPanel(lines, footer, height)
}

func (m *Model) renderPlanGate(height int) string {
	if m.planGate == nil {
		return ""
	}
	contentWidth := max(12, m.width-4)
	meta := "PLAN READY"
	if m.copyToast != "" {
		meta = m.copyToast
	}
	lines := []string{panelHeader(m.styles.Accent.Render("Start implementation"), meta, contentWidth, m.styles)}
	if height <= 7 {
		lines = append(lines,
			m.styles.Muted.Render("Choose the implementation permission mode."),
			renderChoiceRow([]string{"Auto", "Full", "Reject"}, m.planGate.cursor, contentWidth, m.styles),
		)
	} else {
		lines = append(lines,
			m.styles.Agent.Render("The final plan remains visible in the history above."),
			m.styles.Muted.Render("Choose the permission mode for implementation."),
			renderChoiceRow([]string{"Auto", "Full", "Reject"}, m.planGate.cursor, contentWidth, m.styles),
		)
	}
	if m.planGate.editing || strings.TrimSpace(m.planGate.feedback.Value()) != "" {
		lines = append(lines, "", m.styles.Muted.Render("Plan feedback"), m.planGate.feedback.View())
	}
	footer := "←/→ select  ·  Enter confirm  ·  Tab revise plan  ·  Esc exit plan"
	if m.height < 18 {
		footer = "Enter confirm  ·  Tab revise  ·  Esc exit"
	}
	return m.renderBottomPanel(lines, footer, height)
}

func renderChoiceRow(labels []string, selected, width int, styles Styles) string {
	choices := make([]string, len(labels))
	for index, label := range labels {
		text := "  " + label + "  "
		if index == selected {
			text = styles.Accent.Reverse(true).Render(text)
		} else {
			text = styles.Status.Render(text)
		}
		choices[index] = text
	}
	return ansi.Truncate(strings.Join(choices, "  "), width, "...")
}

func panelHeader(title, meta string, width int, styles Styles) string {
	meta = styles.Status.Render(truncateColumns(meta, max(0, width-lipgloss.Width(title)-1)))
	gap := max(1, width-lipgloss.Width(title)-lipgloss.Width(meta))
	return truncateColumns(title+strings.Repeat(" ", gap)+meta, width)
}

func (m *Model) renderBottomPanel(lines []string, footer string, height int) string {
	if height <= 0 {
		return ""
	}
	result := []string{m.styles.InputBorder.Render(strings.Repeat("─", m.width))}
	contentRows := max(0, height-2)
	if len(lines) > contentRows {
		lines = lines[:contentRows]
	}
	for _, line := range lines {
		result = append(result, padPanelLine(line, m.width))
	}
	for len(result) < height-1 {
		result = append(result, strings.Repeat(" ", m.width))
	}
	if height > 1 {
		result = append(result, padPanelLine(m.styles.Muted.Render(truncateColumns(footer, max(0, m.width-4))), m.width))
	}
	return strings.Join(result[:min(height, len(result))], "\n")
}

func padPanelLine(value string, width int) string {
	if width <= 0 {
		return ""
	}
	left := min(2, max(0, width-1))
	line := strings.Repeat(" ", left) + value
	return padWidth(truncateColumns(line, width), width)
}

func fitRenderedRows(value string, height int) string {
	if height <= 0 {
		return ""
	}
	lines := strings.Split(value, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func wrapLimited(value string, width, maxLines int) string {
	lines := strings.Split(wrapPlain(value, width), "\n")
	if len(lines) <= maxLines {
		return strings.Join(lines, "\n")
	}
	lines = lines[:maxLines]
	lines[maxLines-1] = truncateColumns(lines[maxLines-1], max(1, width-3)) + "..."
	return strings.Join(lines, "\n")
}

func (m *Model) renderMarkdown(value string) string {
	contentWidth := m.viewportContentWidth()
	if m.noColor || strings.TrimSpace(value) == "" {
		return wrapPlain(value, max(1, contentWidth-2))
	}
	width := max(20, contentWidth-2)
	if m.markdown.renderer == nil || m.markdown.width != width {
		renderer, err := glamour.NewTermRenderer(glamour.WithStyles(eyluMarkdownStyle()), glamour.WithWordWrap(width))
		if err != nil {
			return wrapPlain(value, max(1, contentWidth-2))
		}
		m.markdown.renderer = renderer
		m.markdown.width = width
	}
	rendered, err := m.markdown.renderer.Render(value)
	if err != nil {
		return wrapPlain(value, max(1, contentWidth-2))
	}
	return rewriteLocalTerminalLinks(strings.TrimSpace(rendered), m.snapshot.Workspace)
}

func (m *Model) viewportLeftInset() int {
	return lipgloss.Width(m.input.Prompt)
}

func (m *Model) viewportContentWidth() int {
	return max(1, m.width-m.viewportLeftInset())
}

func indentBlock(value string, width int) string {
	if value == "" || width <= 0 {
		return value
	}
	prefix := strings.Repeat(" ", width)
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		if line != "" {
			lines[index] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}

func rewriteLocalTerminalLinks(rendered, workspace string) string {
	if rendered == "" || workspace == "" {
		return rendered
	}
	const marker = "\x1b]8;"
	var output strings.Builder
	rest := rendered
	for {
		start := strings.Index(rest, marker)
		if start < 0 {
			output.WriteString(rest)
			return output.String()
		}
		output.WriteString(rest[:start])
		sequence := rest[start:]
		end := strings.IndexByte(sequence, '\a')
		if end < 0 {
			output.WriteString(sequence)
			return output.String()
		}
		header := sequence[len(marker):end]
		separator := strings.IndexByte(header, ';')
		if separator < 0 {
			output.WriteString(sequence[:end+1])
			rest = sequence[end+1:]
			continue
		}
		params, target := header[:separator], header[separator+1:]
		if directoryURL, ok := localContainingDirectoryURL(workspace, target); ok {
			target = directoryURL
		}
		output.WriteString(marker + params + ";" + target + "\a")
		rest = sequence[end+1:]
	}
}

func localContainingDirectoryURL(workspace, target string) (string, bool) {
	if workspace == "" || target == "" || strings.HasPrefix(target, "#") {
		return "", false
	}
	pathValue := target
	windowsPath := filepath.FromSlash(pathValue)
	if filepath.VolumeName(windowsPath) == "" {
		parsed, err := url.Parse(target)
		if err != nil || parsed.Scheme != "" || parsed.Host != "" {
			return "", false
		}
		pathValue = parsed.Path
	}
	decoded, err := url.PathUnescape(pathValue)
	if err != nil {
		return "", false
	}
	localPath := filepath.FromSlash(decoded)
	candidates := make([]string, 0, 3)
	if filepath.VolumeName(localPath) != "" {
		candidates = append(candidates, localPath)
	} else {
		relative := strings.TrimLeft(localPath, "/\\")
		if relative == "" {
			return "", false
		}
		candidates = append(candidates, filepath.Join(workspace, relative))
		parts := strings.FieldsFunc(relative, func(character rune) bool { return character == '/' || character == '\\' })
		if len(parts) > 0 && strings.EqualFold(parts[0], filepath.Base(filepath.Clean(workspace))) {
			candidates = append(candidates, filepath.Join(filepath.Dir(filepath.Clean(workspace)), relative))
		}
		if filepath.IsAbs(localPath) {
			candidates = append(candidates, localPath)
		}
	}
	for _, candidate := range candidates {
		info, statErr := os.Stat(candidate)
		if statErr != nil {
			continue
		}
		directory := candidate
		if !info.IsDir() {
			directory = filepath.Dir(candidate)
		}
		return directoryFileURL(directory), true
	}
	return "", false
}

func directoryFileURL(directory string) string {
	absolute, err := filepath.Abs(directory)
	if err == nil {
		directory = absolute
	}
	pathValue := filepath.ToSlash(filepath.Clean(directory))
	if strings.HasPrefix(pathValue, "//") {
		parts := strings.SplitN(strings.TrimPrefix(pathValue, "//"), "/", 2)
		host := parts[0]
		pathValue = "/"
		if len(parts) == 2 {
			pathValue += parts[1]
		}
		pathValue = strings.TrimSuffix(pathValue, "/") + "/"
		return (&url.URL{Scheme: "file", Host: host, Path: pathValue}).String()
	}
	if filepath.VolumeName(directory) != "" && !strings.HasPrefix(pathValue, "/") {
		pathValue = "/" + pathValue
	}
	pathValue = strings.TrimSuffix(pathValue, "/") + "/"
	return (&url.URL{Scheme: "file", Path: pathValue}).String()
}

func stateLabel(state OperationState) string {
	switch state {
	case StateConnecting:
		return "Connecting"
	case StateCompacting:
		return "Compacting"
	case StateFetchingModels:
		return "Fetching models"
	case StateWaitingFirstToken:
		return "Waiting for first token"
	case StateStreaming:
		return "Streaming"
	case StatePreparingTool:
		return "Preparing file change"
	case StateExecutingTool:
		return "Executing tool"
	case StateAwaitingApproval:
		return "Awaiting approval"
	case StateAwaitingInput:
		return "Awaiting input"
	case StateRetryBackoff:
		return "Retrying"
	case StateCancelling:
		return "Cancelling"
	case StateCancelled:
		return "Cancelled"
	case StateInterrupted:
		return "Interrupted"
	default:
		return string(state)
	}
}

func statusHint(state OperationState) string {
	switch state {
	case StateIdle:
		return "Ready when you are"
	case StateConnecting:
		return "Opening a line"
	case StateCompacting:
		return "Preserving task context"
	case StateFetchingModels:
		return "Gathering models"
	case StateWaitingFirstToken:
		return "Thinking it through"
	case StateStreaming:
		return "Putting it into words"
	case StatePreparingTool:
		return "Planning the next move"
	case StateExecutingTool:
		return "Making it happen"
	case StateAwaitingApproval:
		return "Your call"
	case StateAwaitingInput:
		return "Your turn"
	case StateRetryBackoff:
		return "Trying again shortly"
	case StateCancelling:
		return "Wrapping up"
	case StateCancelled, StateInterrupted:
		return "Paused"
	case StateCompleted:
		return "All set"
	case StateFailed:
		return "Needs attention"
	default:
		return "Ready when you are"
	}
}

func (m *Model) renderStatusHint(value string) string {
	switch m.state {
	case StateCompleted:
		return m.styles.Active.Render(value)
	case StateFailed, StateCancelled, StateInterrupted:
		return m.styles.Error.Render(value)
	case StateAwaitingApproval, StateAwaitingInput, StateRetryBackoff:
		return m.styles.Warning.Render(value)
	case StateConnecting, StateCompacting, StateFetchingModels, StateWaitingFirstToken, StateStreaming, StatePreparingTool, StateExecutingTool, StateCancelling:
		return m.styles.Loading.Render(value)
	default:
		return m.styles.Status.Render(value)
	}
}

func activityLabel(state OperationState) string {
	switch state {
	case StateConnecting:
		return "Connecting"
	case StateCompacting:
		return "Compacting"
	case StateFetchingModels:
		return "Fetching models"
	case StateWaitingFirstToken:
		return "Thinking"
	case StateStreaming:
		return "Composing"
	case StatePreparingTool:
		return "Planning change"
	case StateExecutingTool:
		return "Running tool"
	case StateAwaitingApproval:
		return "Awaiting approval"
	case StateAwaitingInput:
		return "Awaiting input"
	case StateRetryBackoff:
		return "Retrying"
	case StateCancelling:
		return "Cancelling"
	default:
		return stateLabel(state)
	}
}

func formatByteCount(value int) string {
	if value < 1024 {
		return fmt.Sprintf("%d B", value)
	}
	if value < 1024*1024 {
		return fmt.Sprintf("%.1f KiB", float64(value)/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(value)/(1024*1024))
}

func formatTokenCount(value int) string {
	if value < 0 {
		value = 0
	}
	switch {
	case value < 1_000:
		return fmt.Sprintf("%d", value)
	case value < 1_000_000:
		return trimCompactDecimal(float64(value)/1_000) + "K"
	default:
		return trimCompactDecimal(float64(value)/1_000_000) + "M"
	}
}

func FormatTokenCount(value int) string { return formatTokenCount(value) }

func formatOptionalTokenCount(value int) string {
	if value <= 0 {
		return "unknown"
	}
	return formatTokenCount(value)
}

func trimCompactDecimal(value float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", value), ".0")
}

func roundedContextPercent(report contextledger.Report) int {
	percent := report.Percent
	if report.ContextWindow > 0 {
		percent = float64(report.TotalTokens) / float64(report.ContextWindow) * 100
	}
	return min(100, max(0, int(math.Round(percent))))
}

func padWidth(value string, width int) string {
	return value + strings.Repeat(" ", max(0, width-lipgloss.Width(value)))
}

func summarizeLine(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	return truncateColumns(value, limit)
}

func wrapPlain(value string, width int) string {
	if width <= 0 {
		return value
	}
	lines := strings.Split(value, "\n")
	wrapped := make([]string, 0, len(lines))
	for _, line := range lines {
		wrapped = append(wrapped, wrapPlainLine(line, width))
	}
	return strings.Join(wrapped, "\n")
}

func wrapPlainLine(value string, width int) string {
	words := strings.Fields(value)
	var output strings.Builder
	lineWidth := 0
	for _, word := range words {
		chunks := splitWordColumns(word, width)
		for chunkIndex, chunk := range chunks {
			chunkWidth := lipgloss.Width(chunk)
			if lineWidth > 0 && (chunkIndex > 0 || lineWidth+1+chunkWidth > width) {
				output.WriteByte('\n')
				lineWidth = 0
			}
			if lineWidth > 0 {
				output.WriteByte(' ')
				lineWidth++
			}
			output.WriteString(chunk)
			lineWidth += chunkWidth
		}
	}
	return output.String()
}

func splitWordColumns(word string, width int) []string {
	if lipgloss.Width(word) <= width {
		return []string{word}
	}
	chunks := make([]string, 0)
	var current strings.Builder
	currentWidth := 0
	for _, character := range word {
		characterWidth := lipgloss.Width(string(character))
		if currentWidth > 0 && currentWidth+characterWidth > width {
			chunks = append(chunks, current.String())
			current.Reset()
			currentWidth = 0
		}
		current.WriteRune(character)
		currentWidth += characterWidth
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}
	return chunks
}

func truncateColumns(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(value) <= width {
		return value
	}
	if width <= 3 {
		return strings.Repeat(".", width)
	}
	runes := []rune(value)
	for len(runes) > 0 && lipgloss.Width(string(runes))+3 > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "..."
}

func truncateMiddleColumns(value string, width int) string {
	if width <= 0 {
		return ""
	}
	total := ansi.StringWidth(value)
	if total <= width {
		return value
	}
	if width <= 3 {
		return strings.Repeat(".", width)
	}
	leftWidth := (width - 3 + 1) / 2
	rightWidth := width - 3 - leftWidth
	return ansi.Cut(value, 0, leftWidth) + "..." + ansi.Cut(value, total-rightWidth, total)
}
