package tui

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/dgraph-io/badger/v4"
)

type ResetModel struct {
	db           *badger.DB
	resetFn      func() error
	width        int
	height       int
	focusedField int // 0 = Yes, 1 = No
	running      bool
	status       string
	completed    bool
}

type resetCompletedMsg struct {
	err error
}

type resetBackToMenuMsg struct {
	status string
}

func NewResetModel(db *badger.DB, resetFn func() error) *ResetModel {
	return &ResetModel{
		db:      db,
		resetFn: resetFn,
		width:   80,
		height:  24,
	}
}

func (m *ResetModel) Resize(width int, height int) {
	m.width = width
	m.height = height
}

func (m ResetModel) Init() tea.Cmd {
	return nil
}

func (m ResetModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case resetCompletedMsg:
		m.running = false
		m.completed = true
		if msg.err != nil {
			m.status = "Error: " + msg.err.Error()
			return m, nil
		}
		m.status = "Database reset complete"
		return m, nil
	case tea.KeyMsg:
		if m.completed {
			switch msg.String() {
			case "enter", "esc", "q":
				return m, backToMenuAfterResetCmd(m.status)
			}
			return m, nil
		}

		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "tab", "left", "right":
			m.focusedField = (m.focusedField + 1) % 2
		case "enter":
			if m.focusedField == 0 {
				if m.running {
					return m, nil
				}
				if m.resetFn == nil {
					m.status = "Error: reset action not configured"
					return m, nil
				}
				m.running = true
				m.status = "Resetting database..."
				return m, runResetCmd(m.resetFn)
			}

			m.completed = true
			m.status = "Reset cancelled. Data unchanged."
			return m, nil
		}
	}

	return m, nil
}

func (m ResetModel) View() tea.View {
	title := TitleStyle.Render("⚠️  Reset Database")

	warning := ErrorStyle.Render("WARNING: This will permanently delete the entire DUES database!\nThis action cannot be undone.")
	confirmHint := InfoStyle.Render("To confirm: select 'Yes, Delete' and press Enter.")

	yesBtn := ButtonStyle.Render("Yes, Delete")
	if m.focusedField == 0 {
		yesBtn = ButtonSelectedStyle.Render("Yes, Delete")
	}

	noBtn := ButtonStyle.Render("No, Cancel")
	if m.focusedField == 1 {
		noBtn = ButtonSelectedStyle.Render("No, Cancel")
	}

	buttons := lipgloss.JoinHorizontal(lipgloss.Center, yesBtn, noBtn)
	if m.completed {
		buttons = lipgloss.JoinHorizontal(lipgloss.Center, ButtonSelectedStyle.Render("Back to Menu"))
	}

	help := HelpStyle.Render("Tab/Left/Right: Navigate | Enter: Confirm selection | esc: Cancel")
	if m.completed {
		help = HelpStyle.Render("Enter/Esc: Back to menu")
	}

	content := lipgloss.JoinVertical(
		lipgloss.Left,
		title,
		"",
		warning,
		"",
		confirmHint,
		"",
		buttons,
		"",
		m.status,
		"",
		help,
	)

	frame := DialogBoxStyle
	if m.width > 0 {
		frame = frame.Width(max(40, m.width-6))
	}
	if m.height > 0 {
		frame = frame.MaxHeight(max(10, m.height-4))
	}

	return tea.NewView(frame.Render(content))
}

func runResetCmd(resetFn func() error) tea.Cmd {
	return func() tea.Msg {
		return resetCompletedMsg{err: resetFn()}
	}
}

func backToMenuAfterResetCmd(status string) tea.Cmd {
	return func() tea.Msg {
		return resetBackToMenuMsg{status: status}
	}
}
