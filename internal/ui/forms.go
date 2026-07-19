package ui

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

const (
	providerFieldName = iota
	providerFieldURL
	providerFieldModel
	providerFieldAdapter
	providerFieldCatalog
	providerFieldContext
	providerFieldKey
	providerFieldCount
)

type providerFormModel struct {
	inputs          []textinput.Model
	focus           int
	originalName    string
	originalCatalog string
	originalContext int
	err             error
}

func newProviderFormModel(item ProviderForm, width int) providerFormModel {
	inputs := make([]textinput.Model, providerFieldCount)
	placeholders := []string{"Provider name", "https://api.example.com/v1", "Model ID", "openai_responses", "models.dev provider", "Context window", "API key"}
	values := []string{item.Name, item.BaseURL, item.Model, item.Adapter, item.CatalogProvider, "", ""}
	if item.ContextWindow > 0 {
		values[providerFieldContext] = strconv.Itoa(item.ContextWindow)
	}
	for index := range inputs {
		inputs[index] = textinput.New()
		inputs[index].Placeholder = placeholders[index]
		inputs[index].SetValue(values[index])
		inputs[index].SetWidth(max(20, width-24))
		inputs[index].CharLimit = 2048
	}
	inputs[providerFieldKey].EchoMode = textinput.EchoPassword
	inputs[providerFieldKey].EchoCharacter = '*'
	inputs[providerFieldAdapter].SetSuggestions([]string{"openai_responses", "openai_chat"})
	inputs[providerFieldAdapter].ShowSuggestions = true
	_ = inputs[0].Focus()
	return providerFormModel{inputs: inputs, originalName: item.OriginalName, originalCatalog: item.CatalogProvider, originalContext: item.ContextWindow}
}

func (m providerFormModel) update(msg tea.Msg) (providerFormModel, tea.Cmd) {
	updated, command := m.inputs[m.focus].Update(msg)
	m.inputs[m.focus] = updated
	return m, command
}

func (m *providerFormModel) moveFocus(delta int) tea.Cmd {
	m.inputs[m.focus].Blur()
	m.focus = (m.focus + delta + len(m.inputs)) % len(m.inputs)
	return m.inputs[m.focus].Focus()
}

func (m *providerFormModel) setWidth(width int) {
	for index := range m.inputs {
		m.inputs[index].SetWidth(max(20, width-24))
	}
}

func (m providerFormModel) value() (ProviderForm, error) {
	contextWindow := 0
	contextWindowSet := false
	contextWindowRemove := false
	if raw := strings.TrimSpace(m.inputs[providerFieldContext].Value()); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return ProviderForm{}, fmt.Errorf("context window must be a non-negative integer")
		}
		contextWindow = parsed
		contextWindowSet = true
	} else if m.originalName != "" && m.originalContext != 0 {
		contextWindowRemove = true
	}
	name := strings.TrimSpace(m.inputs[providerFieldName].Value())
	baseURL := strings.TrimSpace(m.inputs[providerFieldURL].Value())
	model := strings.TrimSpace(m.inputs[providerFieldModel].Value())
	adapter := strings.TrimSpace(m.inputs[providerFieldAdapter].Value())
	catalogProvider := strings.TrimSpace(m.inputs[providerFieldCatalog].Value())
	if name == "" || baseURL == "" || model == "" || adapter == "" {
		return ProviderForm{}, fmt.Errorf("name, base URL, model, and adapter are required")
	}
	parsedURL, err := url.Parse(baseURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" || parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return ProviderForm{}, fmt.Errorf("base URL must be absolute HTTP(S) without query or fragment")
	}
	return ProviderForm{
		OriginalName: m.originalName, Name: name, BaseURL: baseURL, Model: model, Adapter: adapter,
		APIKey: m.inputs[providerFieldKey].Value(), CatalogProvider: catalogProvider, ContextWindow: contextWindow,
		ContextWindowSet: contextWindowSet, ContextWindowRemove: contextWindowRemove,
		CatalogProviderSet: catalogProvider != m.originalCatalog, CatalogProviderRemove: catalogProvider == "" && m.originalCatalog != "",
	}, nil
}

func (m providerFormModel) view(styles Styles) string {
	labels := []string{"Name", "Base URL", "Model", "Adapter", "Catalog", "Context", "API key"}
	var output strings.Builder
	for index, input := range m.inputs {
		label := styles.Status.Render(fmt.Sprintf("%-10s", labels[index]))
		if index == m.focus {
			label = styles.Active.Render(fmt.Sprintf("%-10s", labels[index]))
		}
		fmt.Fprintf(&output, "%s %s\n", label, input.View())
	}
	if m.err != nil {
		fmt.Fprintf(&output, "\n%s", styles.Error.Render(m.err.Error()))
	}
	return output.String()
}
