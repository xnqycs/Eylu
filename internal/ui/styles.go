package ui

import "charm.land/lipgloss/v2"

type Styles struct {
	User        lipgloss.Style
	Agent       lipgloss.Style
	Tool        lipgloss.Style
	Warning     lipgloss.Style
	Error       lipgloss.Style
	Status      lipgloss.Style
	Loading     lipgloss.Style
	Header      lipgloss.Style
	Muted       lipgloss.Style
	Active      lipgloss.Style
	Border      lipgloss.Style
	InputBorder lipgloss.Style
}

func DefaultStyles(noColor bool) Styles {
	if noColor {
		return Styles{
			User: lipgloss.NewStyle().Bold(true), Agent: lipgloss.NewStyle(), Tool: lipgloss.NewStyle(),
			Warning: lipgloss.NewStyle().Bold(true), Error: lipgloss.NewStyle().Bold(true), Status: lipgloss.NewStyle(),
			Loading: lipgloss.NewStyle(), Header: lipgloss.NewStyle().Bold(true), Muted: lipgloss.NewStyle(),
			Active: lipgloss.NewStyle().Bold(true), Border: lipgloss.NewStyle().Border(lipgloss.NormalBorder()),
			InputBorder: lipgloss.NewStyle(),
		}
	}
	return Styles{
		User:        lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#2AA198")),
		Agent:       lipgloss.NewStyle().Foreground(lipgloss.Color("#D7E3E8")),
		Tool:        lipgloss.NewStyle().Foreground(lipgloss.Color("#D99A2B")),
		Warning:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E6B450")),
		Error:       lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E05252")),
		Status:      lipgloss.NewStyle().Foreground(lipgloss.Color("#87939A")),
		Loading:     lipgloss.NewStyle().Foreground(lipgloss.Color("#58A6C7")),
		Header:      lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#F2F5F7")),
		Muted:       lipgloss.NewStyle().Foreground(lipgloss.Color("#6F7B82")),
		Active:      lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#4BBF73")),
		Border:      lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("#45525A")),
		InputBorder: lipgloss.NewStyle().Foreground(lipgloss.Color("#2A7F82")),
	}
}
