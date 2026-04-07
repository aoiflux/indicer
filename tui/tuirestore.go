package tui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/dgraph-io/badger/v4"
)

type RestoreModel struct {
	hashInput    textinput.Model
	pathInput    textinput.Model
	db           *badger.DB
	restoreFn    func(hash string, restorePath string) error
	width        int
	height       int
	restoring    bool
	focusedField int
	status       string
	askAnother   bool
	askChoice    int // 0 = Yes, 1 = No
}

type restoreCompletedMsg struct {
	err error
}

type restoreBackToMenuMsg struct {
	status string
}

type restoreContinueMsg struct{}

func NewRestoreModel(db *badger.DB, restoreFn func(hash string, restorePath string) error) *RestoreModel {
	hashInput := textinput.New()
	hashInput.Placeholder = "Enter file hash..."
	hashInput.Focus()

	pathInput := textinput.New()
	pathInput.Placeholder = "Enter restore path... (default: restored)"

	return &RestoreModel{
		hashInput: hashInput,
		pathInput: pathInput,
		db:        db,
		restoreFn: restoreFn,
		width:     80,
		height:    24,
	}
}

func (m *RestoreModel) Resize(width int, height int) {
	m.width = width
	m.height = height

	inputWidth := width - 18
	if inputWidth < 24 {
		inputWidth = 24
	}
	m.hashInput.SetWidth(inputWidth)
	m.pathInput.SetWidth(inputWidth)
}

func (m RestoreModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m RestoreModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case restoreCompletedMsg:
		m.restoring = false
		if msg.err != nil {
			m.status = "Error: " + msg.err.Error()
		} else {
			m.status = "Restore completed successfully"
			m.askAnother = true
			m.askChoice = 0
			m.hashInput.Blur()
			m.pathInput.Blur()
		}
		return m, nil
	case restoreContinueMsg:
		m.askAnother = false
		m.askChoice = 0
		m.status = "Ready to restore another file"
		m.hashInput.SetValue("")
		m.pathInput.SetValue("")
		m.focusedField = 0
		m.hashInput.Focus()
		m.pathInput.Blur()
		return m, nil
	case tea.KeyMsg:
		if m.askAnother {
			switch msg.String() {
			case "left", "right", "tab":
				m.askChoice = (m.askChoice + 1) % 2
			case "y":
				m.askChoice = 0
				return m, continueRestoreFlow()
			case "n":
				m.askChoice = 1
				return m, backToMenuAfterRestoreCmd(m.status)
			case "enter":
				if m.askChoice == 0 {
					return m, continueRestoreFlow()
				}
				return m, backToMenuAfterRestoreCmd(m.status)
			}
			return m, nil
		}

		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "tab":
			m.focusedField = (m.focusedField + 1) % 3
			if m.focusedField == 0 {
				m.hashInput.Focus()
				m.pathInput.Blur()
			} else if m.focusedField == 1 {
				m.hashInput.Blur()
				m.pathInput.Focus()
			} else {
				m.pathInput.Blur()
			}
		case "enter":
			if m.focusedField == 2 && !m.restoring {
				hash := strings.TrimSpace(m.hashInput.Value())
				rpath := strings.TrimSpace(m.pathInput.Value())
				if hash == "" {
					m.status = "Error: file hash is required"
					return m, nil
				}
				if rpath == "" {
					rpath = "restored"
				}
				if m.restoreFn == nil {
					m.status = "Error: restore action not configured"
					return m, nil
				}
				m.restoring = true
				m.status = "Restoring file... please wait"
				return m, runRestoreCmd(m.restoreFn, hash, rpath)
			}
		}
	}

	if m.focusedField == 0 {
		m.hashInput, cmd = m.hashInput.Update(msg)
	} else if m.focusedField == 1 {
		m.pathInput, cmd = m.pathInput.Update(msg)
	}

	return m, cmd
}

func runRestoreCmd(restoreFn func(hash string, restorePath string) error, hash string, restorePath string) tea.Cmd {
	return func() tea.Msg {
		return restoreCompletedMsg{err: restoreFn(hash, restorePath)}
	}
}

func continueRestoreFlow() tea.Cmd {
	return func() tea.Msg {
		return restoreContinueMsg{}
	}
}

func backToMenuAfterRestoreCmd(status string) tea.Cmd {
	return func() tea.Msg {
		return restoreBackToMenuMsg{status: status}
	}
}

func (m RestoreModel) View() tea.View {
	title := TitleStyle.Render("Restore File from DUES")

	hashLabel := "File Hash:"
	if m.focusedField == 0 {
		hashLabel = SelectedItemStyle.Render(hashLabel)
	}

	hashField := InputStyle.Render(hashLabel + "\n" + m.hashInput.View())

	pathLabel := "Restore Path:"
	if m.focusedField == 1 {
		pathLabel = SelectedItemStyle.Render(pathLabel)
	}

	pathField := InputStyle.Render(pathLabel + "\n" + m.pathInput.View())

	restoreBtn := ButtonStyle.Render("Restore")
	if m.focusedField == 2 {
		restoreBtn = ButtonSelectedStyle.Render("Restore")
	}

	buttons := lipgloss.JoinHorizontal(lipgloss.Left, restoreBtn, ButtonStyle.Render("Cancel"))

	status := ""
	if m.status != "" {
		status = InfoStyle.Render(m.status)
	}

	askPrompt := ""
	if m.askAnother {
		yesBtn := ButtonStyle.Render("Yes")
		noBtn := ButtonStyle.Render("No")
		if m.askChoice == 0 {
			yesBtn = ButtonSelectedStyle.Render("Yes")
		} else {
			noBtn = ButtonSelectedStyle.Render("No")
		}

		askPrompt = lipgloss.JoinVertical(
			lipgloss.Left,
			InfoStyle.Render("Do you want to restore another file?"),
			lipgloss.JoinHorizontal(lipgloss.Left, yesBtn, noBtn),
		)
	}

	help := HelpStyle.Render("Tab: Navigate | Enter: Select | esc: Back")
	if m.askAnother {
		help = HelpStyle.Render("Left/Right/Tab: Choose | Enter: Confirm | y/n: Quick select")
	}

	content := lipgloss.JoinVertical(
		lipgloss.Left,
		title,
		"",
		hashField,
		pathField,
		"",
		buttons,
		"",
		status,
		"",
		askPrompt,
		"",
		help,
	)

	frame := BorderStyle
	if m.width > 0 {
		frame = frame.Width(max(40, m.width-4))
	}
	if m.height > 0 {
		frame = frame.MaxHeight(max(12, m.height-2))
	}

	return tea.NewView(frame.Render(content))
}
