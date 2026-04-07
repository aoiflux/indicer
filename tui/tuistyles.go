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
			Padding(0, 2).
			MarginBottom(1)

	BorderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(borderColor).
			Padding(1)

	SelectedItemStyle = lipgloss.NewStyle().
				Foreground(primaryColor).
				Background(bgColor).
				Bold(true).
				Padding(0, 1)

	NormalItemStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("250")).
			Padding(0, 1)

	HelpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			Italic(true).
			MarginTop(1)

	InputStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			Padding(1).
			MarginBottom(1)

	SuccessStyle = lipgloss.NewStyle().
			Foreground(successColor).
			Bold(true)

	ErrorStyle = lipgloss.NewStyle().
			Foreground(errorColor).
			Bold(true)

	InfoStyle = lipgloss.NewStyle().
			Foreground(secondaryColor)

	StatusBarStyle = lipgloss.NewStyle().
			Background(bgColor).
			Foreground(lipgloss.Color("250")).
			Padding(0, 1).
			Height(1)

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

	DialogBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.DoubleBorder()).
			BorderForeground(primaryColor).
			Padding(1).
			MarginTop(2).
			MarginBottom(2)
)

// Helper function to create centered boxes
func CenterBox(width int, content string) string {
	return lipgloss.Place(width, 1, lipgloss.Center, lipgloss.Center, content)
}

// Render a titled section
func Section(title string, content string) string {
	return lipgloss.JoinVertical(
		lipgloss.Left,
		TitleStyle.Render(title),
		BorderStyle.Render(content),
	)
}
