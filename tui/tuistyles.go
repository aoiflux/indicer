package tui

import (
	"image/color"
	"indicer/internal/ui"

	"charm.land/lipgloss/v2"
)

// Colours follow the CLI's roles (internal/ui, docs/CLI_UX.md), in their
// dark-background values: the TUI draws on a dark background of its own.
var (
	primaryColor   = role(ui.Forensics) // brand accent: headers, selection
	secondaryColor = role(ui.Highlight) // the active button, info
	successColor   = role(ui.Success)
	warningColor   = role(ui.Warning)
	errorColor     = role(ui.Error)
	accentColor    = role(ui.Evidence) // titles
	mutedColor     = role(ui.Muted)    // dim and help text
	borderColor    = lipgloss.Color("240")
	bgColor        = lipgloss.Color("235")
)

func role(r ui.Role) color.Color { return lipgloss.Color(r.Hex(ui.ThemeDark)) }

// Base styles
var (
	HeaderStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(primaryColor).
			MarginBottom(1)

	TitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(accentColor).
			Background(bgColor).
			Padding(0, 2)

	BorderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(borderColor).
			Padding(0, 1)

	SelectedItemStyle = lipgloss.NewStyle().
				Foreground(primaryColor).
				Background(bgColor).
				Bold(true).
				Padding(0, 1)

	NormalItemStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("250")).
			Padding(0, 1)

	DimStyle = lipgloss.NewStyle().
			Foreground(mutedColor)

	HelpStyle = lipgloss.NewStyle().
			Foreground(mutedColor).
			Italic(true)

	InputStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			Padding(0, 1)

	SuccessStyle = lipgloss.NewStyle().
			Foreground(successColor).
			Bold(true)

	WarningStyle = lipgloss.NewStyle().
			Foreground(warningColor).
			Bold(true)

	ErrorStyle = lipgloss.NewStyle().
			Foreground(errorColor).
			Bold(true)

	InfoStyle = lipgloss.NewStyle().
			Foreground(secondaryColor)

	ButtonStyle = lipgloss.NewStyle().
			Foreground(bgColor).
			Background(primaryColor).
			Padding(0, 2).
			MarginRight(1)

	ButtonSelectedStyle = lipgloss.NewStyle().
				Foreground(bgColor).
				Background(secondaryColor).
				Padding(0, 2).
				MarginRight(1).
				Bold(true)

	ButtonDisabledStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("245")).
				Background(lipgloss.Color("237")).
				Padding(0, 2).
				MarginRight(1)

	DialogBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.DoubleBorder()).
			BorderForeground(primaryColor).
			Padding(1, 2)
)
