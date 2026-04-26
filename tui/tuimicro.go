package tui

import (
	"fmt"
	"strings"

	"indicer/lib/microartefact"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/dgraph-io/badger/v4"
)

type MicroModel struct {
	db       *badger.DB
	dbpath   string
	width    int
	height   int
	loaded   bool
	err      error
	tree     *microartefact.HierarchyTree
	viewport viewport.Model
}

type microLoadedMsg struct {
	tree *microartefact.HierarchyTree
	err  error
}

type microBackToMenuMsg struct{}

func NewMicroModel(db *badger.DB) *MicroModel {
	vp := viewport.New(viewport.WithWidth(76), viewport.WithHeight(20))
	vp.SoftWrap = false
	return &MicroModel{
		db:       db,
		dbpath:   db.Opts().Dir,
		width:    80,
		height:   24,
		viewport: vp,
	}
}

func (m *MicroModel) Resize(width, height int) {
	m.width = width
	m.height = height
	vpWidth := max(50, width-8)
	vpHeight := max(6, height-8)
	m.viewport.SetWidth(vpWidth)
	m.viewport.SetHeight(vpHeight)
}

func (m MicroModel) Init() tea.Cmd {
	return func() tea.Msg {
		repo, err := microartefact.OpenGrapheneRepository(m.dbpath)
		if err != nil {
			return microLoadedMsg{err: err}
		}
		defer repo.Close()

		tree, err := repo.ReadHierarchy()
		return microLoadedMsg{tree: tree, err: err}
	}
}

func (m MicroModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case microLoadedMsg:
		m.loaded = true
		m.err = msg.err
		m.tree = msg.tree
		if m.err == nil {
			m.viewport.SetContent(m.renderTree())
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "q":
			return m, func() tea.Msg { return microBackToMenuMsg{} }
		}
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m MicroModel) View() tea.View {
	title := TitleStyle.Render("Micro-Artefact Graph")

	var body string
	switch {
	case !m.loaded:
		body = InfoStyle.Render("Loading hierarchy...")
	case m.err != nil:
		body = ErrorStyle.Render("Error: " + m.err.Error())
	case m.tree == nil || len(m.tree.DiskImages) == 0:
		body = microDimStyle.Render("No micro-artefacts found. Run dues micro first.")
	default:
		body = m.viewport.View()
	}

	summary := ""
	if m.tree != nil {
		summary = microDimStyle.Render(fmt.Sprintf("%d disk image(s) | %d artefact(s)", len(m.tree.DiskImages), m.tree.TotalArtefacts))
	}

	help := HelpStyle.Render("Up/Down/PgUp/PgDn: Scroll | Esc/q: Back")

	content := lipgloss.JoinVertical(
		lipgloss.Left,
		title,
		summary,
		"",
		body,
		"",
		help,
	)

	frame := DialogBoxStyle
	if m.width > 0 {
		frame = frame.Width(max(50, m.width-6))
	}
	return tea.NewView(frame.Render(content))
}

var microDimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

func (m MicroModel) renderTree() string {
	if m.tree == nil {
		return ""
	}

	var sb strings.Builder
	for _, disk := range m.tree.DiskImages {
		diskName := fallback(disk.Name, disk.ID)
		sb.WriteString(fmt.Sprintf("disk_image: %s\n", diskName))

		for _, part := range disk.Partitions {
			partName := fallback(part.Name, part.ID)
			sb.WriteString(fmt.Sprintf("  partition: %s\n", partName))

			for _, file := range part.Files {
				fileName := fallback(file.Name, file.ID)
				sb.WriteString(fmt.Sprintf("    indexed_file: %s (%d bytes, %d artefacts)\n", fileName, file.Size, len(file.Artefacts)))

				for _, artefact := range file.Artefacts {
					value := artefact.Value
					if len(value) > 100 {
						value = value[:100] + "..."
					}
					sb.WriteString(fmt.Sprintf("      - %s: %s\n", artefact.Kind, value))
				}
			}
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

func fallback(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	if strings.TrimSpace(b) != "" {
		return b
	}
	return "(unknown)"
}
