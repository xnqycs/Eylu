package ui

import (
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
)

func (m *Model) handleProvidersKey(key string) (tea.Model, tea.Cmd) {
	providers := m.snapshot.Providers
	switch key {
	case "esc":
		m.screen = screenChat
	case "up", "k":
		m.providerCursor = clampCursor(m.providerCursor-1, len(providers))
	case "down", "j":
		m.providerCursor = clampCursor(m.providerCursor+1, len(providers))
	case "a":
		m.form = newProviderFormModel(ProviderForm{Adapter: "openai_responses"}, m.viewportContentWidth())
		m.screen = screenProviderForm
	case "e":
		if len(providers) > 0 {
			m.openProviderForm(providers[m.providerCursor])
		}
	case "d":
		if len(providers) > 0 {
			name := providers[m.providerCursor].Name
			return m, func() tea.Msg { return mutationResultMsg{err: m.backend.DeleteProvider(m.context, name)} }
		}
	case "enter":
		if len(providers) > 0 {
			name := providers[m.providerCursor].Name
			return m, func() tea.Msg { return mutationResultMsg{err: m.backend.UseProvider(m.context, name)} }
		}
	case "m":
		if len(providers) > 0 {
			m.snapshot.Provider = providers[m.providerCursor].Name
		}
		m.screen = screenModels
		return m, tea.Batch(m.modelFilter.Focus(), m.fetchModelsCmd())
	}
	return m, nil
}

func (m *Model) handleProviderFormKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	switch key {
	case "esc":
		m.screen = screenProviders
		return m, nil
	case "tab", "enter":
		if m.form.focus == providerFieldCount-1 {
			return m.submitProviderForm()
		}
		return m, m.form.moveFocus(1)
	case "shift+tab":
		return m, m.form.moveFocus(-1)
	case "ctrl+s":
		return m.submitProviderForm()
	}
	updated, command := m.form.update(message)
	m.form = updated
	return m, command
}

func (m *Model) submitProviderForm() (tea.Model, tea.Cmd) {
	value, err := m.form.value()
	if err != nil {
		m.form.err = err
		return m, nil
	}
	m.state = StateConnecting
	return m, func() tea.Msg {
		selection, err := m.backend.UpsertProvider(m.context, value)
		return modelSelectionMsg{selection: selection, returnTo: screenProviderForm, err: err}
	}
}

func (m *Model) handleModelsKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	filtered := m.filteredModels()
	switch key {
	case "esc":
		m.screen = screenChat
		m.modelManual = false
		m.modelFilter.Blur()
		return m, nil
	case "up", "ctrl+p":
		m.modelCursor = clampCursor(m.modelCursor-1, len(filtered))
		return m, nil
	case "down", "ctrl+n":
		m.modelCursor = clampCursor(m.modelCursor+1, len(filtered))
		return m, nil
	case "r":
		m.state = StateFetchingModels
		return m, m.fetchModelsCmd()
	case "m":
		m.modelManual = true
		m.modelFilter.Placeholder = "Manual model ID"
		m.modelFilter.Reset()
		return m, m.modelFilter.Focus()
	case "enter":
		modelID := ""
		if m.modelManual {
			modelID = strings.TrimSpace(m.modelFilter.Value())
		} else if len(filtered) > 0 {
			modelID = filtered[m.modelCursor]
		}
		if modelID != "" {
			m.modelManual = false
			providerName := m.snapshot.Provider
			return m, m.selectModelCmd(providerName, modelID, screenModels)
		}
		return m, nil
	}
	updated, command := m.modelFilter.Update(message)
	m.modelFilter = updated
	m.modelCursor = clampCursor(m.modelCursor, len(m.filteredModels()))
	return m, command
}

func (m *Model) selectModelCmd(providerName, modelID string, returnTo screenKind) tea.Cmd {
	m.state = StateConnecting
	return func() tea.Msg {
		selection, err := m.backend.SetModel(m.context, providerName, modelID)
		return modelSelectionMsg{selection: selection, returnTo: returnTo, err: err}
	}
}

func (m *Model) handleContextWindowConfirmKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	state := m.contextWindowConfirm
	if state == nil {
		m.screen = screenChat
		return m, nil
	}
	key := message.String()
	if state.editing {
		switch key {
		case "esc":
			state.editing = false
			state.input.Blur()
			state.err = ""
			return m, nil
		case "enter":
			value, err := strconv.Atoi(strings.TrimSpace(state.input.Value()))
			if err != nil || value <= 0 {
				state.err = "Context window must be a positive integer."
				return m, nil
			}
			return m, m.setContextWindowCmd(state.selection, value)
		default:
			updated, command := state.input.Update(message)
			state.input = updated
			return m, command
		}
	}
	switch key {
	case "up", "down", "tab", "shift+tab":
		state.cursor = (state.cursor + 1) % 2
	case "y":
		if state.selection.DetectedContextWindow > 0 {
			return m, m.setContextWindowCmd(state.selection, state.selection.DetectedContextWindow)
		}
		return m, m.beginContextWindowInput()
	case "n":
		state.cursor = 1
		return m, m.beginContextWindowInput()
	case "enter":
		if state.cursor == 0 && state.selection.DetectedContextWindow > 0 {
			return m, m.setContextWindowCmd(state.selection, state.selection.DetectedContextWindow)
		}
		return m, m.beginContextWindowInput()
	case "esc":
		m.contextWindowConfirm = nil
		m.screen = screenModels
	}
	return m, nil
}

func (m *Model) beginContextWindowInput() tea.Cmd {
	state := m.contextWindowConfirm
	if state == nil {
		return nil
	}
	state.editing = true
	state.err = ""
	state.input.Reset()
	return state.input.Focus()
}

func (m *Model) setContextWindowCmd(selection ModelSelection, value int) tea.Cmd {
	return func() tea.Msg {
		err := m.backend.SetContextWindow(m.context, selection.Provider, value)
		return contextWindowResultMsg{selection: selection, value: value, err: err}
	}
}

func (m *Model) handleSkillsKey(key string) (tea.Model, tea.Cmd) {
	skills := m.snapshot.Skills
	switch key {
	case "esc":
		m.screen = screenChat
	case "up", "k":
		m.skillCursor = clampCursor(m.skillCursor-1, len(skills))
	case "down", "j":
		m.skillCursor = clampCursor(m.skillCursor+1, len(skills))
	case "enter":
		if len(skills) > 0 && skills[m.skillCursor].Status == "active" {
			return m, m.commandCmd("/skill " + skills[m.skillCursor].Name)
		}
	}
	return m, nil
}

func (m *Model) handleMCPKey(key string) (tea.Model, tea.Cmd) {
	if m.screen == screenMCP {
		switch key {
		case "esc":
			m.screen = screenChat
			m.refreshViewport()
			return m, nil
		case "up", "k":
			m.mcpCursor = clampCursor(m.mcpCursor-1, len(m.mcpServers))
		case "down", "j":
			m.mcpCursor = clampCursor(m.mcpCursor+1, len(m.mcpServers))
		case "enter":
			if len(m.mcpServers) > 0 {
				m.screen = screenMCPDetail
				m.mcpTab = 0
				m.mcpCatalogCursor = 0
				m.refreshViewport()
				m.viewport.GotoTop()
			}
			return m, nil
		case "g":
			return m, m.fetchMCPServersCmd()
		default:
			return m.handleMCPActionKey(key)
		}
		m.refreshViewport()
		return m, nil
	}

	switch key {
	case "esc":
		m.screen = screenMCP
		m.mcpCatalogCursor = 0
		m.refreshViewport()
		m.viewport.GotoTop()
		return m, nil
	case "right", "tab":
		m.mcpTab = (m.mcpTab + 1) % 4
		m.mcpCatalogCursor = 0
	case "left", "shift+tab":
		m.mcpTab = (m.mcpTab + 3) % 4
		m.mcpCatalogCursor = 0
	case "1", "2", "3", "4":
		m.mcpTab = int(key[0] - '1')
		m.mcpCatalogCursor = 0
	case "up", "k":
		m.mcpCatalogCursor = clampCursor(m.mcpCatalogCursor-1, m.selectedMCPCatalogLength())
	case "down", "j":
		m.mcpCatalogCursor = clampCursor(m.mcpCatalogCursor+1, m.selectedMCPCatalogLength())
	case "enter":
		if m.mcpTab == 1 && m.selectedMCPCatalogLength() > 0 {
			m.screen = screenMCPToolDetail
			m.refreshViewport()
			m.viewport.GotoTop()
			return m, nil
		}
	case "g":
		return m, m.fetchMCPServersCmd()
	default:
		return m.handleMCPActionKey(key)
	}
	m.refreshViewport()
	return m, nil
}

func (m *Model) handleMCPToolDetailKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case "esc":
		m.screen = screenMCPDetail
		m.refreshViewport()
		m.viewport.GotoTop()
		return m, nil
	case "g":
		return m, m.fetchMCPServersCmd()
	default:
		updated, command := m.viewport.Update(message)
		m.viewport = updated
		return m, command
	}
}

func (m *Model) handleMCPActionKey(key string) (tea.Model, tea.Cmd) {
	action := MCPAction("")
	switch key {
	case "r":
		action = MCPActionReconnect
	case "e":
		action = MCPActionEnable
	case "d":
		action = MCPActionDisable
	case "l":
		action = MCPActionLogin
	case "o":
		action = MCPActionLogout
	}
	server, ok := m.selectedMCPServer()
	if action == "" || !ok {
		return m, nil
	}
	m.mcpNotice = server.Name + ": " + string(action) + "..."
	m.mcpNoticeError = false
	m.refreshViewport()
	return m, func() tea.Msg {
		err := m.backend.MCPAction(m.context, server.Name, action)
		return mcpActionResultMsg{server: server.Name, action: action, err: err}
	}
}

func (m *Model) selectedMCPServer() (MCPServerItem, bool) {
	if len(m.mcpServers) == 0 {
		return MCPServerItem{}, false
	}
	m.mcpCursor = clampCursor(m.mcpCursor, len(m.mcpServers))
	return m.mcpServers[m.mcpCursor], true
}

func (m *Model) selectedMCPCatalogLength() int {
	server, ok := m.selectedMCPServer()
	if !ok {
		return 0
	}
	switch m.mcpTab {
	case 1:
		return len(server.Tools)
	case 2:
		return len(server.Resources)
	case 3:
		return len(server.Prompts)
	default:
		return 0
	}
}

func (m *Model) selectedMCPTool() (MCPServerItem, MCPToolItem, bool) {
	server, ok := m.selectedMCPServer()
	if !ok || len(server.Tools) == 0 {
		return MCPServerItem{}, MCPToolItem{}, false
	}
	m.mcpCatalogCursor = clampCursor(m.mcpCatalogCursor, len(server.Tools))
	return server, server.Tools[m.mcpCatalogCursor], true
}

func (m *Model) openProviderForm(item ProviderItem) {
	m.form = newProviderFormModel(ProviderForm{
		OriginalName: item.Name, Name: item.Name, BaseURL: item.BaseURL, Model: item.Model,
		Adapter: item.Adapter, ContextWindow: item.ContextWindow,
	}, m.viewportContentWidth())
	m.screen = screenProviderForm
}

func (m *Model) providerByName(name string) (ProviderItem, bool) {
	for _, item := range m.snapshot.Providers {
		if item.Name == name {
			return item, true
		}
	}
	return ProviderItem{}, false
}

func (m *Model) fetchModelsCmd() tea.Cmd {
	providerName := m.snapshot.Provider
	m.state = StateFetchingModels
	return func() tea.Msg {
		models, err := m.backend.FetchModels(m.context, providerName)
		sort.Strings(models)
		return modelsResultMsg{models: models, err: err}
	}
}

func (m *Model) filteredModels() []string {
	query := strings.ToLower(strings.TrimSpace(m.modelFilter.Value()))
	if query == "" || m.modelManual {
		return m.models
	}
	result := make([]string, 0)
	for _, model := range m.models {
		if strings.Contains(strings.ToLower(model), query) {
			result = append(result, model)
		}
	}
	return result
}

func clampCursor(value, count int) int {
	if count <= 0 {
		return 0
	}
	if value < 0 {
		return count - 1
	}
	if value >= count {
		return 0
	}
	return value
}
