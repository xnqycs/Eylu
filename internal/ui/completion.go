package ui

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const (
	maxCompletionRows       = 8
	maxCompletionCandidates = 200
)

type completionKind int

const (
	completionNone completionKind = iota
	completionSlash
	completionReference
)

type completionItem struct {
	label       string
	description string
	insert      string
	disabled    bool
	rank        int
}

type completionState struct {
	kind   completionKind
	start  int
	end    int
	cursor int
	items  []completionItem
}

type filesResultMsg struct {
	files []FileItem
	err   error
}

func (m *Model) refreshCompletion() tea.Cmd {
	value := m.input.Value()
	kind, start, query, ok := completionContext(value)
	if !ok {
		m.completion = completionState{}
		m.updateViewportHeight()
		return nil
	}
	state := completionState{kind: kind, start: start, end: len(value)}
	needFiles := false
	switch kind {
	case completionSlash:
		state.items = slashCompletionItems(value, m.snapshot)
	case completionReference:
		state.items, needFiles = referenceCompletionItems(query, m.snapshot, m.files, m.filesLoaded, m.filesLoading, m.filesDiagnostic)
	}
	if len(state.items) == 0 {
		state.items = []completionItem{{label: "No matches", disabled: true}}
	}
	state.cursor = firstEnabledCompletion(state.items)
	m.completion = state
	m.updateViewportHeight()
	if needFiles && !m.filesLoading {
		m.filesLoading = true
		return m.loadFilesCmd()
	}
	return nil
}

func completionContext(value string) (completionKind, int, string, bool) {
	if strings.HasPrefix(value, "/") && !strings.Contains(value, "\n") {
		return completionSlash, 0, value, true
	}
	start := len(value)
	for start > 0 {
		r, size := previousRune(value[:start])
		if unicode.IsSpace(r) {
			break
		}
		start -= size
	}
	if start < len(value) && value[start] == '@' {
		return completionReference, start, value[start+1:], true
	}
	return completionNone, 0, "", false
}

func previousRune(value string) (rune, int) {
	return utf8.DecodeLastRuneInString(value)
}

func slashCompletionItems(value string, snapshot Snapshot) []completionItem {
	commands := []completionItem{
		{label: "/context", description: "Inspect context usage", insert: "/context"},
		{label: "/help", description: "Show available commands", insert: "/help"},
		{label: "/mode", description: "Change permission mode", insert: "/mode "},
		{label: "/model", description: "Choose the active model", insert: "/model"},
		{label: "/new", description: "Start a new session", insert: "/new"},
		{label: "/provider", description: "Manage model providers", insert: "/provider "},
		{label: "/providers", description: "Browse configured providers", insert: "/providers"},
		{label: "/quit", description: "Exit Eylu", insert: "/quit"},
		{label: "/skill", description: "Activate an Agent Skill", insert: "/skill "},
		{label: "/skills", description: "Browse Agent Skills", insert: "/skills"},
		{label: "/tasks", description: "Inspect the current task list", insert: "/tasks"},
	}
	lower := strings.ToLower(value)
	switch {
	case strings.HasPrefix(lower, "/mode "):
		query := strings.TrimSpace(strings.TrimPrefix(lower, "/mode "))
		return filterCompletionItems([]completionItem{
			{label: "manual", description: "Confirm writes and commands", insert: "/mode manual"},
			{label: "plan", description: "Run the isolated planning agent", insert: "/mode plan"},
			{label: "auto", description: "Auto-run approved workspace actions", insert: "/mode auto"},
			{label: "full", description: "Auto-run ordinary actions", insert: "/mode full"},
		}, query)
	case strings.HasPrefix(lower, "/provider "):
		query := strings.TrimSpace(strings.TrimPrefix(lower, "/provider "))
		return filterCompletionItems([]completionItem{
			{label: "add", description: "Create a provider", insert: "/provider add"},
			{label: "edit", description: "Edit a provider", insert: "/provider edit "},
			{label: "use", description: "Activate a provider", insert: "/provider use "},
			{label: "delete", description: "Delete a provider", insert: "/provider delete "},
		}, query)
	case strings.HasPrefix(lower, "/skill "):
		query := strings.TrimSpace(strings.TrimPrefix(value, "/skill "))
		items := make([]completionItem, 0, len(snapshot.Skills))
		for _, skill := range snapshot.Skills {
			if skill.Status == "active" {
				items = append(items, completionItem{label: skill.Name, description: skill.Description, insert: "/skill " + skill.Name})
			}
		}
		return filterCompletionItems(items, query)
	case strings.Contains(strings.TrimPrefix(value, "/"), " "):
		return nil
	}
	query := strings.TrimPrefix(value, "/")
	items := append([]completionItem(nil), commands...)
	for _, skill := range snapshot.Skills {
		if skill.Status == "active" {
			items = append(items, completionItem{label: "/" + skill.Name, description: skill.Description, insert: "/skill " + skill.Name, rank: 1})
		}
	}
	return filterCompletionItems(items, query)
}

func referenceCompletionItems(query string, snapshot Snapshot, files []FileItem, loaded, loading bool, diagnostic string) ([]completionItem, bool) {
	kind := ReferenceKind("")
	search := query
	if strings.HasPrefix(strings.ToLower(search), "skill:") {
		kind = ReferenceSkill
		search = search[len("skill:"):]
	} else if strings.HasPrefix(strings.ToLower(search), "file:") {
		kind = ReferenceFile
		search = search[len("file:"):]
	}
	items := make([]completionItem, 0, min(maxCompletionCandidates, len(files)+len(snapshot.Skills)))
	appendMatch := func(item completionItem) {
		if len(items) >= maxCompletionCandidates || (!item.disabled && !completionCandidateMatches(item.label, search)) {
			return
		}
		items = append(items, item)
	}
	if kind == "" || kind == ReferenceSkill {
		for _, skill := range snapshot.Skills {
			if skill.Status != "active" {
				continue
			}
			appendMatch(completionItem{
				label: "@skill:" + skill.Name, description: skill.Description,
				insert: referenceToken(ReferenceSkill, skill.Name),
			})
		}
	}
	needFiles := kind == "" || kind == ReferenceFile
	if needFiles && diagnostic != "" {
		appendMatch(completionItem{label: "Files unavailable", description: diagnostic, disabled: true, rank: 2})
		needFiles = false
	} else if needFiles && loaded {
		for _, file := range files {
			appendMatch(completionItem{
				label: "@file:" + file.Path, description: "File  " + formatByteCount(int(file.Size)),
				insert: referenceToken(ReferenceFile, file.Path), rank: 1,
			})
		}
	} else if needFiles {
		label := "Loading files..."
		if !loading {
			label = "Indexing files..."
		}
		appendMatch(completionItem{label: label, disabled: true, rank: 2})
	}
	return filterCompletionItems(items, search), needFiles && !loaded
}

func completionCandidateMatches(label, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	candidate := strings.ToLower(strings.TrimPrefix(label, "/"))
	candidate = strings.TrimPrefix(candidate, "@skill:")
	candidate = strings.TrimPrefix(candidate, "@file:")
	return strings.Contains(candidate, query)
}

func filterCompletionItems(items []completionItem, query string) []completionItem {
	query = strings.ToLower(strings.TrimSpace(query))
	result := make([]completionItem, 0, min(len(items), maxCompletionCandidates))
	for _, item := range items {
		candidate := strings.ToLower(strings.TrimPrefix(item.label, "/"))
		candidate = strings.TrimPrefix(candidate, "@skill:")
		candidate = strings.TrimPrefix(candidate, "@file:")
		switch {
		case item.disabled:
			item.rank += 4
		case query == "":
		case strings.HasPrefix(candidate, query):
		case strings.Contains(candidate, query):
			item.rank += 2
		default:
			continue
		}
		result = append(result, item)
		if len(result) >= maxCompletionCandidates {
			break
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].rank != result[j].rank {
			return result[i].rank < result[j].rank
		}
		return strings.ToLower(result[i].label) < strings.ToLower(result[j].label)
	})
	return result
}

func firstEnabledCompletion(items []completionItem) int {
	for index, item := range items {
		if !item.disabled {
			return index
		}
	}
	return 0
}

func (m *Model) handleCompletionKey(key string) (bool, tea.Cmd) {
	if m.completion.kind == completionNone || len(m.completion.items) == 0 {
		return false, nil
	}
	switch key {
	case "esc":
		m.completion = completionState{}
		m.updateViewportHeight()
		return true, nil
	case "up", "ctrl+p":
		m.moveCompletion(-1)
		return true, nil
	case "down", "ctrl+n":
		m.moveCompletion(1)
		return true, nil
	case "tab", "enter":
		item := m.completion.items[m.completion.cursor]
		if item.disabled {
			return key == "tab", nil
		}
		value := m.input.Value()
		value = value[:m.completion.start] + item.insert + value[m.completion.end:]
		m.input.SetValue(value)
		m.input.MoveToEnd()
		m.resetHistoryNavigation()
		m.completion = completionState{}
		m.updateViewportHeight()
		return true, nil
	default:
		return false, nil
	}
}

func (m *Model) moveCompletion(delta int) {
	if len(m.completion.items) == 0 {
		return
	}
	for attempts := 0; attempts < len(m.completion.items); attempts++ {
		m.completion.cursor = clampCursor(m.completion.cursor+delta, len(m.completion.items))
		if !m.completion.items[m.completion.cursor].disabled {
			return
		}
	}
}

func (m *Model) completionHeight() int {
	if m.completion.kind == completionNone {
		return 0
	}
	available := max(0, m.height-m.input.Height()-fixedChromeRows-minViewportRows-m.taskPanelRows())
	return min(len(m.completion.items), min(maxCompletionRows, available))
}

func (m *Model) renderCompletion() string {
	height := m.completionHeight()
	if height == 0 {
		return ""
	}
	start := 0
	if m.completion.cursor >= height {
		start = m.completion.cursor - height + 1
	}
	end := min(len(m.completion.items), start+height)
	lines := make([]string, 0, end-start)
	for index := start; index < end; index++ {
		item := m.completion.items[index]
		marker := "  "
		if index == m.completion.cursor && !item.disabled {
			marker = "> "
		}
		markerWidth := lipgloss.Width(marker)
		labelWidth := min(max(12, m.width/2), max(0, m.width-markerWidth))
		label := truncateColumns(item.label, labelWidth)
		descriptionWidth := max(0, m.width-markerWidth-lipgloss.Width(label)-2)
		description := truncateColumns(strings.Join(strings.Fields(item.description), " "), descriptionWidth)
		line := marker + label
		if description != "" {
			line += strings.Repeat(" ", max(2, m.width-lipgloss.Width(line)-lipgloss.Width(description))) + m.styles.Muted.Render(description)
		}
		line = padWidth(truncateColumns(line, m.width), m.width)
		if index == m.completion.cursor && !item.disabled {
			line = m.styles.Active.Render(line)
		} else if item.disabled {
			line = m.styles.Muted.Render(line)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (m *Model) loadFilesCmd() tea.Cmd {
	return func() tea.Msg {
		files, err := m.backend.ListFiles(m.context)
		return filesResultMsg{files: files, err: err}
	}
}

func referenceToken(kind ReferenceKind, value string) string {
	prefix := "@" + string(kind) + ":"
	if strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return prefix + strconv.Quote(value)
	}
	return prefix + value
}

func parseReferences(value string) []Reference {
	result := make([]Reference, 0)
	seen := make(map[string]struct{})
	for index := 0; index < len(value); index++ {
		if index > 0 {
			previous, _ := utf8.DecodeLastRuneInString(value[:index])
			if !unicode.IsSpace(previous) {
				continue
			}
		}
		kind := ReferenceKind("")
		prefix := ""
		switch {
		case strings.HasPrefix(value[index:], "@skill:"):
			kind, prefix = ReferenceSkill, "@skill:"
		case strings.HasPrefix(value[index:], "@file:"):
			kind, prefix = ReferenceFile, "@file:"
		case value[index] == '@':
			kind, prefix = ReferenceFile, "@"
		default:
			continue
		}
		start := index + len(prefix)
		parsed, consumed := parseReferenceValue(value[start:])
		if consumed == 0 || parsed == "" {
			continue
		}
		key := string(kind) + "\x00" + parsed
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			result = append(result, Reference{Kind: kind, Value: parsed})
		}
		index = start + consumed - 1
	}
	return result
}

func parseReferenceValue(value string) (string, int) {
	if value == "" {
		return "", 0
	}
	if value[0] == '"' {
		escaped := false
		for index := 1; index < len(value); index++ {
			if escaped {
				escaped = false
				continue
			}
			if value[index] == '\\' {
				escaped = true
				continue
			}
			if value[index] == '"' {
				quoted := value[:index+1]
				parsed, err := strconv.Unquote(quoted)
				if err == nil {
					return parsed, index + 1
				}
				return "", 0
			}
		}
		return "", 0
	}
	end := strings.IndexFunc(value, unicode.IsSpace)
	if end < 0 {
		end = len(value)
	}
	return value[:end], end
}
