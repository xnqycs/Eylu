package ui

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type markdownRenderCache struct {
	renderer *glamour.TermRenderer
	width    int
}

const (
	activityGapRows = 1
	fixedChromeRows = 5 + activityGapRows
	minViewportRows = 4
)

type tuiLayout struct {
	viewportTop     int
	viewportHeight  int
	completionRows  int
	inputContentRow int
	panelHeight     int
}

func (m *Model) layout() tuiLayout {
	layout := tuiLayout{viewportTop: 1}
	if m.approval != nil || m.planGate != nil {
		layout.panelHeight = m.decisionPanelHeight()
		layout.viewportHeight = max(minViewportRows, m.height-layout.viewportTop-layout.panelHeight)
		return layout
	}
	layout.completionRows = m.completionHeight()
	layout.viewportHeight = max(minViewportRows, m.height-m.input.Height()-fixedChromeRows-layout.completionRows)
	layout.inputContentRow = layout.viewportTop + layout.viewportHeight + layout.completionRows + activityGapRows + 2
	return layout
}

func (m *Model) decisionPanelHeight() int {
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
		parts = append(parts, renderBlankRows(m.width, activityGapRows), m.renderLoading(), input, m.renderStatus())
	}
	content := fitRenderedRows(strings.Join(parts, "\n"), m.height)
	if m.noColor {
		content = ansi.Strip(content)
	}
	view := tea.NewView(content)
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	view.WindowTitle = "Eylu"
	if m.screen == screenChat && m.approval == nil && m.planGate == nil {
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
	m.modelFilter.SetWidth(max(20, m.width-6))
	m.approvalReason.SetWidth(max(20, m.width-12))
	if m.planGate != nil {
		m.planGate.feedback.SetWidth(max(20, m.width-12))
	}
	m.form.setWidth(m.width)
	m.viewport.SetWidth(m.width)
	m.updateViewportHeight()
	m.refreshViewport()
}

func (m *Model) updateViewportHeight() {
	reservedCompletion := 0
	if m.completion.kind != completionNone {
		reservedCompletion = 1
	}
	inputLimit := min(maxInputRows, max(1, m.height-fixedChromeRows-minViewportRows-reservedCompletion))
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
	wasBottom := m.viewport.AtBottom() || m.followOutput
	var content string
	switch m.screen {
	case screenProviders:
		content = m.renderProviders()
	case screenProviderForm:
		content = m.form.view(m.styles)
	case screenModels:
		content = m.renderModels()
	case screenSkills:
		content = m.renderSkills()
	case screenContext:
		content = m.renderContext()
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
	title := m.styles.Header.Render("Eylu")
	available := max(8, m.width-lipgloss.Width(title)-1)
	meta := m.styles.Status.Render(truncateColumns(fmt.Sprintf("%s  %s", provider, model), available))
	space := max(1, m.width-lipgloss.Width(title)-lipgloss.Width(meta))
	return title + strings.Repeat(" ", space) + meta
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
	elapsed := time.Duration(0)
	if !m.startedAt.IsZero() {
		elapsed = max(time.Duration(0), m.clock.Now().Sub(m.startedAt)).Round(time.Second)
	}
	details := []string{elapsed.String(), m.renderInputActivity(), m.renderTokenActivity()}
	if thinking := m.renderThinkingActivity(); thinking != "" {
		details = append(details, thinking)
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
	switch m.state {
	case StateWaitingFirstToken, StateStreaming, StatePreparingTool:
	default:
		return ""
	}
	bytesPerToken := m.activity.TokenBytesPerToken
	if bytesPerToken <= 0 {
		bytesPerToken = 4
	}
	if m.roundReasoningExact {
		if m.roundReasoningTokens > 0 {
			return fmt.Sprintf("thinking %d tokens", m.roundReasoningTokens)
		}
		return ""
	}
	if m.reasoningBytes > 0 {
		estimatedTokens := (m.reasoningBytes + bytesPerToken - 1) / bytesPerToken
		return fmt.Sprintf("thinking ≈%d tokens", estimatedTokens)
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
	report := m.snapshot.Context
	contextText := fmt.Sprintf("%d tokens", report.TotalTokens)
	if report.LimitKnown {
		contextText = fmt.Sprintf("%.1f%% context", report.Percent)
	}
	left := fmt.Sprintf("%s  %s", mode, contextText)
	right := truncateColumns(string(m.state), max(0, m.width-1))
	left = truncateColumns(left, max(0, m.width-lipgloss.Width(right)-1))
	space := max(0, m.width-lipgloss.Width(left)-lipgloss.Width(right))
	return m.styles.Status.Render(left + strings.Repeat(" ", space) + right)
}

func (m *Model) renderTimeline() string {
	var output strings.Builder
	for index := range m.timeline {
		item := &m.timeline[index]
		switch item.kind {
		case timelineMessage:
			if item.role == "user" {
				fmt.Fprintf(&output, "%s\n%s\n\n", m.styles.User.Render("YOU"), wrapPlain(item.text, m.width-2))
			} else {
				fmt.Fprintf(&output, "%s\n%s\n\n", m.styles.Agent.Render("EYLU"), m.renderTimelineMarkdown(item))
			}
		case timelineTool:
			fmt.Fprintf(&output, "%s\n", m.renderTool(item.tool))
		case timelineNotice:
			style := m.styles.Status
			if item.err {
				style = m.styles.Error
			}
			fmt.Fprintf(&output, "%s\n\n", style.Render(wrapPlain(item.text, m.width-2)))
		}
	}
	return strings.TrimRight(output.String(), "\n")
}

func (m *Model) renderTimelineMarkdown(item *timelineItem) string {
	if item == nil {
		return ""
	}
	if item.renderedSource == item.text && item.renderedWidth == m.width &&
		item.renderedWorkspace == m.snapshot.Workspace && item.renderedNoColor == m.noColor {
		return item.renderedText
	}
	item.renderedSource = item.text
	item.renderedWidth = m.width
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
	}
	duration := ""
	if tool.durationMS > 0 {
		duration = "  " + FormatDurationMS(tool.durationMS)
	}
	detail := summarizeLine(tool.arguments, max(20, m.width-30))
	if tool.path != "" {
		detail = fmt.Sprintf("%s  %s  %d lines", m.renderFileLocationLink(tool.path), formatByteCount(tool.generatedBytes), tool.generatedLines)
	}
	lines := []string{fmt.Sprintf("> %s  %s%s", tool.name, state, duration)}
	if detail != "" {
		lines = append(lines, "  "+ansi.Truncate(detail, max(10, m.width-2), "..."))
	}
	if tool.preview != "" {
		for _, line := range strings.Split(tool.preview, "\n") {
			lines = append(lines, "  "+truncateColumns(line, max(10, m.width-2)))
		}
	}
	return m.styles.Tool.Render(strings.Join(lines, "\n"))
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
	if m.noColor || path == "" {
		return path
	}
	directoryURL, ok := localContainingDirectoryURL(m.snapshot.Workspace, path)
	if !ok {
		return path
	}
	return ansi.SetHyperlink(directoryURL) + path + ansi.ResetHyperlink()
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
		line = truncateColumns(line, m.width)
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
	for index, model := range m.filteredModels() {
		cursor := "  "
		if index == m.modelCursor {
			cursor = "> "
		}
		line := cursor + truncateColumns(model, max(8, m.width-3))
		if model == m.snapshot.Model {
			line = m.styles.Active.Render(line)
		}
		output.WriteString(line + "\n")
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
		line = truncateColumns(line, m.width)
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

func (m *Model) renderContext() string {
	report := m.snapshot.Context
	var output strings.Builder
	limit := "unknown"
	if report.LimitKnown {
		limit = fmt.Sprintf("%d", report.ContextWindow)
	}
	fmt.Fprintf(&output, "%s\n%d input + %d reserved / %s\n\n", m.styles.Header.Render("Context"), report.InputTokens, report.OutputReserve, limit)
	for _, category := range report.Categories {
		bar := progressBar(category.Percent, 18)
		fmt.Fprintf(&output, "%-22s %6d  %s %5.1f%%  %s\n", category.Label, category.Tokens, bar, category.Percent, category.Measurement)
		if m.contextExpand {
			for _, source := range category.Sources {
				fmt.Fprintf(&output, "  %-20s %6d  %5.1f%%\n", truncateColumns(source.Source, 20), source.Tokens, source.Percent)
			}
		}
	}
	if report.LastUsage.InputTokens > 0 || report.LastUsage.OutputTokens > 0 {
		fmt.Fprintf(&output, "\nProvider usage  %d input  %d output", report.LastUsage.InputTokens, report.LastUsage.OutputTokens)
	}
	return output.String()
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
	if m.noColor || strings.TrimSpace(value) == "" {
		return wrapPlain(value, m.width-2)
	}
	width := max(20, m.width-2)
	if m.markdown.renderer == nil || m.markdown.width != width {
		renderer, err := glamour.NewTermRenderer(glamour.WithStyles(eyluMarkdownStyle()), glamour.WithWordWrap(width))
		if err != nil {
			return wrapPlain(value, m.width-2)
		}
		m.markdown.renderer = renderer
		m.markdown.width = width
	}
	rendered, err := m.markdown.renderer.Render(value)
	if err != nil {
		return wrapPlain(value, m.width-2)
	}
	return rewriteLocalTerminalLinks(strings.TrimSpace(rendered), m.snapshot.Workspace)
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

func activityLabel(state OperationState) string {
	switch state {
	case StateConnecting:
		return "Connecting"
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

func progressBar(percent float64, width int) string {
	filled := int(percent / 100 * float64(width))
	filled = min(width, max(0, filled))
	return "[" + strings.Repeat("=", filled) + strings.Repeat(" ", width-filled) + "]"
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
