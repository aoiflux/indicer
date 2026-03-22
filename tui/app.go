package tui

import (
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/dgraph-io/badger/v4"
)

type Model struct {
	width        int
	height       int
	db           *badger.DB
	actions      Actions
	state        State
	list         list.Model
	storeModel   *StoreModel
	listModel    *ListModel
	searchModel  *SearchModel
	restoreModel *RestoreModel
	nearModel    *NearModel
	resetModel   *ResetModel
	menuStatus   string
	err          error
	prevState    State
}

type Actions struct {
	Store   func(filePath string, syncIndex bool, noIndex bool) error
	Search  func(query string) error
	Restore func(hash string, restorePath string) error
	NearIn  func(hash string, deep bool) error
	NearOut func(filePath string) error
	Reset   func() error
}

type State int

const (
	StateMenu State = iota
	StateStore
	StateRestore
	StateList
	StateSearch
	StateNear
	StateReset
	StateLoading
	StateError
	StateSuccess
)

type Item struct {
	title       string
	description string
	action      State
}

func (i Item) FilterValue() string { return i.title }

type itemDelegate struct{}

func (d itemDelegate) Height() int                               { return 2 }
func (d itemDelegate) Spacing() int                              { return 0 }
func (d itemDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd { return nil }
func (d itemDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	i := item.(Item)

	str := fmt.Sprintf("  %s\n  %s", i.title, i.description)

	if index == m.Index() {
		fmt.Fprint(w, SelectedItemStyle.Render(str))
	} else {
		fmt.Fprint(w, NormalItemStyle.Render(str))
	}
}

// NewModel creates a new TUI model
func NewModel(db *badger.DB, actions Actions) *Model {
	items := []list.Item{
		Item{
			title:       "💾 Store File",
			description: "Store a file in the DUES database",
			action:      StateStore,
		},
		Item{
			title:       "📂 List Files",
			description: "List all files in the database",
			action:      StateList,
		},
		Item{
			title:       "🔍 Search",
			description: "Search for content in the database",
			action:      StateSearch,
		},
		Item{
			title:       "🔄 Restore File",
			description: "Restore a file from the database",
			action:      StateRestore,
		},
		Item{
			title:       "🔎 NeAR Analysis",
			description: "Find similar files using NeAR",
			action:      StateNear,
		},
		Item{
			title:       "🗑️  Reset Database",
			description: "Delete the entire database",
			action:      StateReset,
		},
	}

	const defaultWidth = 80

	l := list.New(items, itemDelegate{}, defaultWidth, 15)
	l.Title = "DUES - Deduplicated Unified Evidence Store"
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)

	return &Model{
		width:        defaultWidth,
		height:       24,
		state:        StateMenu,
		list:         l,
		db:           db,
		actions:      actions,
		storeModel:   NewStoreModel(db, actions.Store),
		listModel:    NewListModel(db, actions.Restore),
		searchModel:  NewSearchModel(db, actions.Search),
		restoreModel: NewRestoreModel(db, actions.Restore),
		nearModel:    NewNearModel(db, actions.NearIn, actions.NearOut),
		resetModel:   NewResetModel(db, actions.Reset),
	}
}

func (m Model) Init() tea.Cmd {
	return nil
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case storeBackToMenuMsg:
		m.state = StateMenu
		m.menuStatus = msg.status
		m.storeModel = NewStoreModel(m.db, m.actions.Store)
		m.applyWindowSize()
		return m, nil
	case restoreBackToMenuMsg:
		m.state = StateMenu
		m.menuStatus = msg.status
		m.restoreModel = NewRestoreModel(m.db, m.actions.Restore)
		m.applyWindowSize()
		return m, nil
	case resetBackToMenuMsg:
		m.state = StateMenu
		m.menuStatus = msg.status
		m.resetModel = NewResetModel(m.db, m.actions.Reset)
		m.applyWindowSize()
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q":
			if m.state == StateMenu {
				return m, tea.Quit
			}
			if m.state == StateReset && m.resetModel != nil && m.resetModel.completed {
				break
			}
			// Otherwise 'q' acts as escape in other screens
			fallthrough
		case "esc":
			if m.state != StateMenu {
				if m.state == StateReset && m.resetModel != nil && m.resetModel.completed {
					break
				}
				// Return to menu
				m.state = StateMenu
				// Reset submodels
				m.storeModel = NewStoreModel(m.db, m.actions.Store)
				m.listModel = NewListModel(m.db, m.actions.Restore)
				m.searchModel = NewSearchModel(m.db, m.actions.Search)
				m.restoreModel = NewRestoreModel(m.db, m.actions.Restore)
				m.nearModel = NewNearModel(m.db, m.actions.NearIn, m.actions.NearOut)
				m.resetModel = NewResetModel(m.db, m.actions.Reset)
				m.applyWindowSize()
				return m, nil
			}
		case "enter":
			if m.state == StateMenu {
				selected := m.list.SelectedItem().(Item)
				m.prevState = m.state
				m.state = selected.action
				m.applyWindowSize()
				switch m.state {
				case StateList:
					m.listModel = NewListModel(m.db, m.actions.Restore)
					m.listModel.Resize(m.width, m.height)
					return m, m.listModel.Init()
				case StateNear:
					return m, m.nearModel.Init()
				default:
					// Avoid key carry-over into child screen on the same Enter press.
					return m, nil
				}
			}
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.applyWindowSize()
	}

	// Update the appropriate submodel based on current state
	switch m.state {
	case StateMenu:
		m.list, cmd = m.list.Update(msg)
	case StateStore:
		updated, updateCmd := m.storeModel.Update(msg)
		if model, ok := updated.(StoreModel); ok {
			m.storeModel = &model
		}
		cmd = updateCmd
	case StateList:
		updated, updateCmd := m.listModel.Update(msg)
		if model, ok := updated.(ListModel); ok {
			m.listModel = &model
		}
		cmd = updateCmd
	case StateSearch:
		updated, updateCmd := m.searchModel.Update(msg)
		if model, ok := updated.(SearchModel); ok {
			m.searchModel = &model
		}
		cmd = updateCmd
	case StateRestore:
		updated, updateCmd := m.restoreModel.Update(msg)
		if model, ok := updated.(RestoreModel); ok {
			m.restoreModel = &model
		}
		cmd = updateCmd
	case StateNear:
		updated, updateCmd := m.nearModel.Update(msg)
		if model, ok := updated.(NearModel); ok {
			m.nearModel = &model
		}
		cmd = updateCmd
	case StateReset:
		updated, updateCmd := m.resetModel.Update(msg)
		if model, ok := updated.(ResetModel); ok {
			m.resetModel = &model
		}
		cmd = updateCmd
	}

	return m, cmd
}

func (m *Model) applyWindowSize() {
	if m.width <= 0 || m.height <= 0 {
		return
	}

	listWidth := m.width - 8
	if listWidth < 40 {
		listWidth = 40
	}
	listHeight := m.height - 10
	if listHeight < 8 {
		listHeight = 8
	}
	m.list.SetWidth(listWidth)
	m.list.SetHeight(listHeight)

	m.storeModel.Resize(m.width, m.height)
	m.listModel.Resize(m.width, m.height)
	m.searchModel.Resize(m.width, m.height)
	m.restoreModel.Resize(m.width, m.height)
	m.nearModel.Resize(m.width, m.height)
	m.resetModel.Resize(m.width, m.height)
}

func (m Model) View() tea.View {
	var v tea.View
	switch m.state {
	case StateMenu:
		v = tea.NewView(m.viewMenu())
	case StateStore:
		v = m.storeModel.View()
	case StateList:
		v = m.listModel.View()
	case StateSearch:
		v = m.searchModel.View()
	case StateRestore:
		v = m.restoreModel.View()
	case StateNear:
		v = m.nearModel.View()
	case StateReset:
		v = m.resetModel.View()
	case StateError:
		v = tea.NewView(m.viewError())
	case StateSuccess:
		v = tea.NewView(m.viewSuccess())
	case StateLoading:
		v = tea.NewView(m.viewLoading())
	default:
		v = tea.NewView(m.viewMenu())
	}
	v.AltScreen = true
	return v
}

func (m Model) viewMenu() string {
	header := HeaderStyle.Render("DUES - Deduplicated Unified Evidence Store")
	subheader := InfoStyle.Render(fmt.Sprintf("Database: %s", m.db.Opts().Dir))
	banner := ""
	if strings.TrimSpace(m.menuStatus) != "" {
		if strings.HasPrefix(strings.ToLower(m.menuStatus), "error:") {
			banner = ErrorStyle.Render("Last action: " + m.menuStatus)
		} else {
			banner = SuccessStyle.Render("Last action: " + m.menuStatus)
		}
	}

	listView := m.list.View()

	help := HelpStyle.Render("↑/↓: Navigate | Enter: Select | q: Quit | ctrl+c: Exit")

	content := lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		subheader,
		banner,
		"",
		listView,
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

	return frame.Render(content)
}

func (m Model) viewError() string {
	return DialogBoxStyle.Render(
		ErrorStyle.Render("Error") + "\n\n" +
			fmt.Sprintf("%v", m.err) + "\n\n" +
			HelpStyle.Render("Press any key to continue..."),
	)
}

func (m Model) viewSuccess() string {
	return DialogBoxStyle.Render(
		SuccessStyle.Render("Success") + "\n\n" +
			"Operation completed successfully!" + "\n\n" +
			HelpStyle.Render("Press any key to continue..."),
	)
}

func (m Model) viewLoading() string {
	return CenterBox(m.width, "⏳ Loading...")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// RunTUI starts the interactive TUI
func RunTUI(db *badger.DB, actions Actions) error {
	m := NewModel(db, actions)
	p := tea.NewProgram(m)
	_, err := p.Run()
	return err
}
