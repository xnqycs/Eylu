package ui

import (
	glamouransi "charm.land/glamour/v2/ansi"
	glamourstyles "charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
)

const (
	eyluAccentColor       = "#35BDB2"
	eyluTextColor         = "#D7E1E5"
	eyluToolColor         = "#E0A33A"
	eyluWarningColor      = "#E6B95C"
	eyluDangerColor       = "#E36D6D"
	eyluMutedColor        = "#78878E"
	eyluActivityColor     = "#58B9D0"
	eyluSuccessColor      = "#68C28B"
	eyluBorderColor       = "#344A50"
	eyluSelectionColor    = "#83D6CD"
	eyluSelectionInkColor = "#071315"
)

var eyluAccentRGB = [3]uint8{0x35, 0xBD, 0xB2}

type Styles struct {
	Accent      lipgloss.Style
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
	Selection   lipgloss.Style
	Border      lipgloss.Style
	InputBorder lipgloss.Style
}

func DefaultStyles(noColor bool) Styles {
	if noColor {
		return Styles{
			Accent: lipgloss.NewStyle().Bold(true), User: lipgloss.NewStyle().Bold(true), Agent: lipgloss.NewStyle(), Tool: lipgloss.NewStyle(),
			Warning: lipgloss.NewStyle().Bold(true), Error: lipgloss.NewStyle().Bold(true), Status: lipgloss.NewStyle(),
			Loading: lipgloss.NewStyle(), Header: lipgloss.NewStyle().Bold(true), Muted: lipgloss.NewStyle(),
			Active: lipgloss.NewStyle().Bold(true), Selection: lipgloss.NewStyle().Reverse(true), Border: lipgloss.NewStyle().Border(lipgloss.NormalBorder()),
			InputBorder: lipgloss.NewStyle(),
		}
	}
	// Eylu Signal: a restrained terminal palette where teal carries focus,
	// amber carries tool activity, and green/red are reserved for decisions.
	return Styles{
		Accent:      lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(eyluAccentColor)),
		User:        lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(eyluAccentColor)),
		Agent:       lipgloss.NewStyle().Foreground(lipgloss.Color(eyluTextColor)),
		Tool:        lipgloss.NewStyle().Foreground(lipgloss.Color(eyluToolColor)),
		Warning:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(eyluWarningColor)),
		Error:       lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(eyluDangerColor)),
		Status:      lipgloss.NewStyle().Foreground(lipgloss.Color(eyluMutedColor)),
		Loading:     lipgloss.NewStyle().Foreground(lipgloss.Color(eyluActivityColor)),
		Header:      lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(eyluTextColor)),
		Muted:       lipgloss.NewStyle().Foreground(lipgloss.Color(eyluMutedColor)),
		Active:      lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(eyluSuccessColor)),
		Selection:   lipgloss.NewStyle().Foreground(lipgloss.Color(eyluSelectionInkColor)).Background(lipgloss.Color(eyluSelectionColor)),
		Border:      lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color(eyluBorderColor)),
		InputBorder: lipgloss.NewStyle().Foreground(lipgloss.Color(eyluAccentColor)),
	}
}

func eyluMarkdownStyle() glamouransi.StyleConfig {
	style := glamourstyles.DarkStyleConfig
	style.Document.Color = stringPointer(eyluTextColor)
	style.BlockQuote.Color = stringPointer(eyluMutedColor)
	style.Heading.Color = stringPointer(eyluAccentColor)
	style.H1.Color = stringPointer(eyluAccentColor)
	style.H1.BackgroundColor = nil
	style.H1.Prefix = ""
	style.H1.Suffix = ""
	style.H6.Color = stringPointer(eyluMutedColor)
	style.HorizontalRule.Color = stringPointer(eyluBorderColor)
	style.Link.Color = stringPointer(eyluActivityColor)
	style.LinkText.Color = stringPointer(eyluAccentColor)
	style.Code.Color = stringPointer(eyluAccentColor)
	style.Code.BackgroundColor = nil
	return style
}

func stringPointer(value string) *string { return &value }
