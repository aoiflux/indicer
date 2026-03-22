package tui

import (
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/dgraph-io/badger/v4"
)

type NearModel struct {
	modeList     list.Model // in/out
	fileInput    textinput.Model
	deepEnabled  bool
	db           *badger.DB
	nearInFn     func(hash string, deep bool) error
	nearOutFn    func(filePath string) error
	width        int
	height       int
	analyzing    bool
	focusedField int
	status       string
	mode         string // "in" or "out"
}

type nearCompletedMsg struct {
	err error
}

type NearItem struct {
	title string
	desc  string
	mode  string
}

func (i NearItem) FilterValue() string { return i.title }

type nearDelegate struct{}

func (d nearDelegate) Height() int                               { return 2 }
func (d nearDelegate) Spacing() int                              { return 0 }
func (d nearDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd { return nil }
func (d nearDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	i := item.(NearItem)
	str := fmt.Sprintf("  %s\n  %s", i.title, i.desc)
	if index == m.Index() {
		fmt.Fprint(w, SelectedItemStyle.Render(str))
	} else {
		fmt.Fprint(w, NormalItemStyle.Render(str))
	}
}

func NewNearModel(db *badger.DB, nearInFn func(hash string, deep bool) error, nearOutFn func(filePath string) error) *NearModel {
	items := []list.Item{
		NearItem{
			title: "📂 Find in Database",
			desc:  "Analyze files already in DUES database",
			mode:  "in",
		},
		NearItem{
			title: "📁 Find from File",
			desc:  "Analyze an external file against database",
			mode:  "out",
		},
	}

	modeList := list.New(items, nearDelegate{}, 60, 8)
	modeList.Title = "NeAR Analysis Mode"
	modeList.SetShowStatusBar(false)

	fileInput := textinput.New()
	fileInput.Placeholder = "Enter hash or file path..."
	fileInput.Focus()

	return &NearModel{
		modeList:    modeList,
		fileInput:   fileInput,
		db:          db,
		nearInFn:    nearInFn,
		nearOutFn:   nearOutFn,
		width:       80,
		height:      24,
		deepEnabled: false,
	}
}

func (m *NearModel) Resize(width int, height int) {
	m.width = width
	m.height = height

	innerWidth := width - 12
	if innerWidth < 32 {
		innerWidth = 32
	}
	listHeight := height - 14
	if listHeight < 6 {
		listHeight = 6
	}
	m.modeList.SetWidth(innerWidth)
	m.modeList.SetHeight(listHeight)

	inputWidth := width - 18
	if inputWidth < 24 {
		inputWidth = 24
	}
	m.fileInput.SetWidth(inputWidth)
}

func (m NearModel) Init() tea.Cmd {
	return nil
}

func (m NearModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case nearCompletedMsg:
		m.analyzing = false
		if msg.err != nil {
			m.status = "Error: " + msg.err.Error()
		} else {
			m.status = "Analysis completed successfully"
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "tab":
			if m.focusedField == 0 {
				m.focusedField = 1
				m.fileInput.Focus()
			} else {
				m.focusedField = 0
				m.fileInput.Blur()
			}
		case "enter":
			if m.focusedField == 0 {
				item := m.modeList.SelectedItem().(NearItem)
				m.mode = item.mode
				m.focusedField = 1
				m.fileInput.Focus()
			} else if m.focusedField == 1 && !m.analyzing {
				input := strings.TrimSpace(m.fileInput.Value())
				if input == "" {
					m.status = "Error: input is required"
					return m, nil
				}

				var run tea.Cmd
				switch m.mode {
				case "in":
					if m.nearInFn == nil {
						m.status = "Error: near-in action not configured"
						return m, nil
					}
					run = runNearInCmd(m.nearInFn, input, m.deepEnabled)
				case "out":
					if m.nearOutFn == nil {
						m.status = "Error: near-out action not configured"
						return m, nil
					}
					run = runNearOutCmd(m.nearOutFn, input)
				default:
					m.status = "Error: mode not selected"
					return m, nil
				}

				m.analyzing = true
				m.status = "Analyzing... this may take a moment"
				return m, run
			}
		case "d":
			if m.focusedField == 1 {
				m.deepEnabled = !m.deepEnabled
			}
		}
	}

	if m.focusedField == 0 {
		m.modeList, cmd = m.modeList.Update(msg)
	} else if m.focusedField == 1 {
		m.fileInput, cmd = m.fileInput.Update(msg)
	}

	return m, cmd
}

func runNearInCmd(nearInFn func(hash string, deep bool) error, hash string, deep bool) tea.Cmd {
	return func() tea.Msg {
		return nearCompletedMsg{err: nearInFn(hash, deep)}
	}
}

func runNearOutCmd(nearOutFn func(filePath string) error, filePath string) tea.Cmd {
	return func() tea.Msg {
		return nearCompletedMsg{err: nearOutFn(filePath)}
	}
}

func (m NearModel) View() tea.View {
	title := TitleStyle.Render("NeAR - Find Similar Files")

	var content string

	if m.mode == "" {
		// Select mode
		content = lipgloss.JoinVertical(
			lipgloss.Left,
			title,
			"",
			m.modeList.View(),
			"",
			HelpStyle.Render("↑/↓: Select | Enter: Choose | esc: Back"),
		)
	} else {
		// Input mode
		inputLabel := "Input:"
		if m.focusedField == 1 {
			inputLabel = SelectedItemStyle.Render(inputLabel)
		}

		deepToggle := "☐ Deep Scan (slower, finds partial matches)"
		if m.deepEnabled {
			deepToggle = "☑ Deep Scan (slower, finds partial matches)"
		}

		analyzeBtn := ButtonStyle.Render("Analyze")
		if m.focusedField == 1 {
			analyzeBtn = ButtonSelectedStyle.Render("Analyze")
		}

		status := ""
		if m.status != "" {
			status = InfoStyle.Render(m.status)
		}

		content = lipgloss.JoinVertical(
			lipgloss.Left,
			title,
			"",
			InputStyle.Render(inputLabel+"\n"+m.fileInput.View()),
			"",
			deepToggle,
			"",
			analyzeBtn,
			"",
			status,
			"",
			HelpStyle.Render("d: Toggle Deep | Tab: Navigate | esc: Back"),
		)
	}

	frame := BorderStyle
	if m.width > 0 {
		frame = frame.Width(max(40, m.width-4))
	}
	if m.height > 0 {
		frame = frame.MaxHeight(max(12, m.height-2))
	}

	return tea.NewView(frame.Render(content))
}
