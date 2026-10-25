package tui

import (
	"charm.land/lipgloss/v2"
)

// Color scheme
var (
	primaryColor   = lipgloss.Color("205") // Magenta
	secondaryColor = lipgloss.Color("39")  // Cyan
	successColor   = lipgloss.Color("42")  // Green
	warningColor   = lipgloss.Color("214") // Orange
	errorColor     = lipgloss.Color("196") // Red
	accentColor    = lipgloss.Color("227") // Yellow
	borderColor    = lipgloss.Color("240") // Dark gray
	bgColor        = lipgloss.Color("235") // Very dark gray
)

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
			Foreground(borderColor)

	HelpStyle = lipgloss.NewStyle().
			Foreground(borderColor).
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
