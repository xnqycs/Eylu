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

func (m *Model) View() tea.View {
	if m.screen != screenChat {
		m.refreshViewport()
	}
	header := m.renderHeader()
	loading := m.renderLoading()
	status := m.renderStatus()
	input := strings.Repeat("\n", 2)
	if m.screen == screenChat {
		input = m.renderInputBand()
	}
	content := strings.Join([]string{header, m.viewport.View(), loading, input, status}, "\n")
	if m.approval != nil {
		content = m.renderApproval(content)
	}
	if m.noColor {
		content = ansi.Strip(content)
	}
	view := tea.NewView(content)
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	view.WindowTitle = "Eylu"
	if m.screen == screenChat && m.approval == nil {
		view.Cursor = m.input.Cursor()
		if view.Cursor != nil {
			view.Cursor.Position.Y += lipgloss.Height(header) + m.viewport.Height() + lipgloss.Height(loading) + 1
		}
	}
	return view
}

func (m *Model) renderInputBand() string {
	rule := m.styles.InputBorder.Render(strings.Repeat("─", m.width))
	input := strings.TrimRight(m.input.View(), "\n")
	return strings.Join([]string{rule, input, rule}, "\n")
}

func (m *Model) resize(width, height int) {
	m.width = max(40, width)
	m.height = max(12, height)
	m.input.SetWidth(m.width)
	m.input.SetHeight(1)
	m.modelFilter.SetWidth(max(20, m.width-6))
	m.form.setWidth(m.width)
	m.viewport.SetWidth(m.width)
	m.viewport.SetHeight(max(4, m.height-6))
	m.refreshViewport()
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
	if !m.busy() {
		return strings.Repeat(" ", max(1, m.width))
	}
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
	rendered := ansi.Truncate(lead+"  "+metadata, m.width, "...")
	return padWidth(rendered, m.width)
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
	report := m.snapshot.Context
	contextText := fmt.Sprintf("%d tokens", report.TotalTokens)
	if report.LimitKnown {
		contextText = fmt.Sprintf("%.1f%% context", report.Percent)
	}
	left := fmt.Sprintf("%s  %s", mode, contextText)
	right := string(m.state)
	space := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right))
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
		duration = fmt.Sprintf("  %dms", tool.durationMS)
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

func (m *Model) renderApproval(base string) string {
	approval := m.approval
	if approval == nil {
		return base
	}
	style := m.styles.Warning
	if approval.Warning {
		style = m.styles.Error
	}
	content := fmt.Sprintf("Approval %d/%d\n%s  %s\n%s\n%s", approval.Step, approval.Total, approval.Tool, approval.Risk, approval.Reason, approval.Summary)
	modal := m.styles.Border.Width(min(m.width-8, 72)).Render(style.Render(content))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, modal)
}

func (m *Model) renderMarkdown(value string) string {
	if m.noColor || strings.TrimSpace(value) == "" {
		return wrapPlain(value, m.width-2)
	}
	width := max(20, m.width-2)
	if m.markdown.renderer == nil || m.markdown.width != width {
		renderer, err := glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(width))
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
	if lipgloss.Width(value) <= width {
		return value
	}
	runes := []rune(value)
	for len(runes) > 0 && lipgloss.Width(string(runes))+3 > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "..."
}
