package tui

import (
	"strings"

	"indicer/lib/cnst"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/dgraph-io/badger/v4"
)

type storeCompletedMsg struct {
	err error
}

type storeBackToMenuMsg struct {
	status string
}

type StoreModel struct {
	fileInput    textinput.Model
	db           *badger.DB
	storeFn      func(filePath string, syncIndex bool, noIndex bool, hashAlgo string) error
	width        int
	height       int
	status       string
	syncIndex    bool
	noIndex      bool
	hashAlgo     string // cnst.SHA3 or cnst.BLAKE3
	focusedField int
	running      bool
	askAnother   bool
	askChoice    int // 0 = Yes, 1 = No
}

func NewStoreModel(db *badger.DB, storeFn func(filePath string, syncIndex bool, noIndex bool, hashAlgo string) error) *StoreModel {
	fileInput := textinput.New()
	fileInput.Placeholder = "Enter file path..."
	fileInput.Focus()

	return &StoreModel{
		fileInput: fileInput,
		db:        db,
		storeFn:   storeFn,
		width:     80,
		height:    24,
		hashAlgo:  cnst.SHA3,
	}
}

func (m *StoreModel) Resize(width int, height int) {
	m.width = width
	m.height = height

	inputWidth := width - 18
	if inputWidth < 24 {
		inputWidth = 24
	}
	m.fileInput.SetWidth(inputWidth)
}

func (m StoreModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m StoreModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case storeCompletedMsg:
		m.running = false
		if msg.err != nil {
			m.status = "Error: " + msg.err.Error()
		} else {
			m.status = "Stored successfully"
			m.askAnother = true
			m.askChoice = 0
			m.fileInput.Blur()
		}
		return m, nil
	case storeContinueMsg:
		m.askAnother = false
		m.askChoice = 0
		m.status = "Ready to store another file"
		m.fileInput.SetValue("")
		m.focusedField = 0
		m.fileInput.Focus()
		return m, nil
	case tea.KeyMsg:
		if m.askAnother {
			switch msg.String() {
			case "left", "right", "tab":
				m.askChoice = (m.askChoice + 1) % 2
			case "y":
				m.askChoice = 0
				return m, continueStoreFlow()
			case "n":
				m.askChoice = 1
				return m, backToMenuCmd(m.status)
			case "enter":
				if m.askChoice == 0 {
					return m, continueStoreFlow()
				}
				return m, backToMenuCmd(m.status)
			}
			return m, nil
		}

		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "tab":
			m.focusedField = (m.focusedField + 1) % 6
			if m.focusedField == 0 {
				m.fileInput.Focus()
			} else {
				m.fileInput.Blur()
			}
		case "enter":
			switch m.focusedField {
			case 1:
				m.syncIndex = !m.syncIndex
				if m.syncIndex {
					m.noIndex = false
				}
			case 2:
				m.noIndex = !m.noIndex
				if m.noIndex {
					m.syncIndex = false
				}
			case 3:
				if m.hashAlgo == cnst.SHA3 {
					m.hashAlgo = cnst.BLAKE3
				} else {
					m.hashAlgo = cnst.SHA3
				}
			case 4:
				if m.running {
					return m, nil
				}

				path := strings.TrimSpace(m.fileInput.Value())
				if path == "" {
					m.status = "Error: file path is required"
					return m, nil
				}
				if m.storeFn == nil {
					m.status = "Error: store action not configured"
					return m, nil
				}

				m.status = "Storing file... please wait"
				m.running = true
				return m, runStoreCmd(m.storeFn, path, m.syncIndex, m.noIndex, m.hashAlgo)
			case 5:
				return m, tea.Quit
			}
		}
	}

	if m.focusedField == 0 {
		m.fileInput, cmd = m.fileInput.Update(msg)
	}

	return m, cmd
}

func continueStoreFlow() tea.Cmd {
	return func() tea.Msg {
		return storeContinueMsg{}
	}
}

type storeContinueMsg struct{}

func runStoreCmd(storeFn func(filePath string, syncIndex bool, noIndex bool, hashAlgo string) error, filePath string, syncIndex bool, noIndex bool, hashAlgo string) tea.Cmd {
	return func() tea.Msg {
		err := storeFn(filePath, syncIndex, noIndex, hashAlgo)
		return storeCompletedMsg{err: err}
	}
}

func backToMenuCmd(status string) tea.Cmd {
	return func() tea.Msg {
		return storeBackToMenuMsg{status: status}
	}
}

func (m StoreModel) View() tea.View {
	title := TitleStyle.Render("Store File in DUES Database")

	fileLabel := "File Path:"
	if m.focusedField == 0 {
		fileLabel = SelectedItemStyle.Render(fileLabel)
	}

	fileField := InputStyle.Render(fileLabel + "\n" + m.fileInput.View())

	syncLabel := "☐ Sync Index"
	if m.syncIndex {
		syncLabel = "☑ Sync Index"
	}
	if m.focusedField == 1 {
		syncLabel = SelectedItemStyle.Render(syncLabel)
	}

	noIndexLabel := "☐ Skip Index"
	if m.noIndex {
		noIndexLabel = "☑ Skip Index"
	}
	if m.focusedField == 2 {
		noIndexLabel = SelectedItemStyle.Render(noIndexLabel)
	}

	sha3Badge := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true).Render("SHA3")
	blake3Badge := lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true).Render("BLAKE3")
	var hashLabel string
	if m.hashAlgo == cnst.SHA3 {
		hashLabel = "Hash Algo: " + sha3Badge + lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(" (BLAKE3)")
	} else {
		hashLabel = "Hash Algo: " + blake3Badge + lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(" (SHA3)")
	}
	if m.focusedField == 3 {
		hashLabel = SelectedItemStyle.Render("Hash Algo:") + " " + func() string {
			if m.hashAlgo == cnst.BLAKE3 {
				return blake3Badge + lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(" (SHA3)")
			}
			return sha3Badge + lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(" (BLAKE3)")
		}()
	}

	options := lipgloss.JoinVertical(lipgloss.Left, syncLabel, noIndexLabel, hashLabel)

	storeBtn := ButtonStyle.Render("Store")
	if m.focusedField == 4 {
		storeBtn = ButtonSelectedStyle.Render("Store")
	}
	cancelBtn := ButtonStyle.Render("Cancel")
	if m.focusedField == 5 {
		cancelBtn = ButtonSelectedStyle.Render("Cancel")
	}

	buttons := lipgloss.JoinHorizontal(lipgloss.Left, storeBtn, cancelBtn)

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
			InfoStyle.Render("Do you want to store another file?"),
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
		fileField,
		"",
		"Options:",
		options,
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
