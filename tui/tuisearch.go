package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/dgraph-io/badger/v4"
)

type SearchModel struct {
	queryInput   textinput.Model
	results      []string
	db           *badger.DB
	searchFn     func(query string) error
	width        int
	height       int
	searching    bool
	focusedField int
}

type searchCompletedMsg struct {
	err error
}

func NewSearchModel(db *badger.DB, searchFn func(query string) error) *SearchModel {
	queryInput := textinput.New()
	queryInput.Placeholder = "Enter search query..."
	queryInput.Focus()

	return &SearchModel{
		queryInput: queryInput,
		db:         db,
		searchFn:   searchFn,
		width:      80,
		height:     24,
	}
}

func (m *SearchModel) Resize(width int, height int) {
	m.width = width
	m.height = height

	inputWidth := width - 18
	if inputWidth < 24 {
		inputWidth = 24
	}
	m.queryInput.SetWidth(inputWidth)
}

func (m SearchModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m SearchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case searchCompletedMsg:
		m.searching = false
		if msg.err != nil {
			m.results = []string{"Error: " + msg.err.Error()}
		} else {
			m.results = []string{"Search complete", "Generated report.json in current directory"}
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "enter":
			if m.focusedField == 0 && !m.searching {
				query := strings.TrimSpace(m.queryInput.Value())
				if query == "" {
					m.results = []string{"Error: query is required"}
					return m, nil
				}
				if m.searchFn == nil {
					m.results = []string{"Error: search action not configured"}
					return m, nil
				}
				m.searching = true
				m.results = nil
				return m, runSearchCmd(m.searchFn, query)
			}
		}
	}

	if m.focusedField == 0 {
		m.queryInput, cmd = m.queryInput.Update(msg)
	}

	return m, cmd
}

func runSearchCmd(searchFn func(query string) error, query string) tea.Cmd {
	return func() tea.Msg {
		return searchCompletedMsg{err: searchFn(query)}
	}
}

func (m SearchModel) View() tea.View {
	title := TitleStyle.Render("Search DUES Database")

	queryLabel := "Search Query:"
	if m.focusedField == 0 {
		queryLabel = SelectedItemStyle.Render(queryLabel)
	}

	queryField := InputStyle.Render(queryLabel + "\n" + m.queryInput.View())

	var resultsView string
	if m.searching {
		resultsView = "🔍 Searching..."
	} else if len(m.results) > 0 {
		resultsView = "Results:\n"
		for i, r := range m.results {
			resultsView += fmt.Sprintf("%d. %s\n", i+1, r)
		}
	} else if m.queryInput.Value() != "" {
		resultsView = InfoStyle.Render("No results found")
	}

	help := HelpStyle.Render("Enter: Search | esc: Back | ctrl+c: Exit")

	content := lipgloss.JoinVertical(
		lipgloss.Left,
		title,
		"",
		queryField,
		"",
		resultsView,
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
