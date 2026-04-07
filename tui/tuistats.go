package tui

import (
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/store"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/dgraph-io/badger/v4"
	"github.com/dustin/go-humanize"
)

type StatsModel struct {
	db       *badger.DB
	width    int
	height   int
	stats    *store.DBStats
	err      error
	loaded   bool
	viewport viewport.Model
}

type statsLoadedMsg struct {
	stats *store.DBStats
	err   error
}

func NewStatsModel(db *badger.DB) *StatsModel {
	vp := viewport.New(viewport.WithWidth(76), viewport.WithHeight(20))
	vp.SoftWrap = true
	return &StatsModel{
		db:       db,
		width:    80,
		height:   24,
		viewport: vp,
	}
}

func (m *StatsModel) Resize(width, height int) {
	m.width = width
	m.height = height
	vpWidth := max(50, width-8)
	vpHeight := max(6, height-8) // leave room for title + help
	m.viewport.SetWidth(vpWidth)
	m.viewport.SetHeight(vpHeight)
}

func (m StatsModel) Init() tea.Cmd {
	return func() tea.Msg {
		s, err := store.GatherStats(m.db)
		return statsLoadedMsg{stats: s, err: err}
	}
}

func (m StatsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case statsLoadedMsg:
		m.loaded = true
		m.stats = msg.stats
		m.err = msg.err
		if m.err == nil {
			m.viewport.SetContent(m.renderStats())
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "q":
			return m, backToMenuFromStatsCmd("")
		}
	}
	// Forward all other messages (arrow keys, page up/down, etc.) to viewport
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

type statsBackToMenuMsg struct {
	status string
}

func backToMenuFromStatsCmd(status string) tea.Cmd {
	return func() tea.Msg {
		return statsBackToMenuMsg{status: status}
	}
}

func (m StatsModel) View() tea.View {
	title := TitleStyle.Render("📊 Database Statistics")

	var body string
	if !m.loaded {
		body = InfoStyle.Render("Loading statistics…")
	} else if m.err != nil {
		body = ErrorStyle.Render("Error: " + m.err.Error())
	} else {
		pct := int(m.viewport.ScrollPercent() * 100)
		scrollInfo := statDimStyle.Render(fmt.Sprintf(" %d%%", pct))
		body = m.viewport.View() + "\n" + scrollInfo
	}

	help := HelpStyle.Render("↑/↓/PgUp/PgDn: Scroll | Esc/q: Back to menu")

	content := lipgloss.JoinVertical(
		lipgloss.Left,
		title,
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

var (
	statSectionStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	statLabelStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Width(32)
	statValStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("227")).Bold(true)
	statGoodStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	statDimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

func statRow(label, value string, valStyle lipgloss.Style) string {
	return statLabelStyle.Render(label+":") + " " + valStyle.Render(value)
}

func (m StatsModel) renderStats() string {
	s := m.stats
	sep := statDimStyle.Render("  " + "─────────────────────────────────────────────")

	lines := []string{}

	// FILES
	lines = append(lines, statSectionStyle.Render("  FILES"))
	lines = append(lines, sep)
	lines = append(lines, statRow("  Total files", humanize.Comma(s.TotalFiles), statValStyle))
	lines = append(lines, statRow("  Completed", humanize.Comma(s.CompletedFiles), statGoodStyle))
	if inc := s.TotalFiles - s.CompletedFiles; inc > 0 {
		lines = append(lines, statRow("  Incomplete", humanize.Comma(inc), lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)))
	}
	lines = append(lines, statRow("  Total logical size", humanize.Bytes(uint64(s.TotalLogicalSize)), statValStyle))
	if s.CompletedFiles > 0 {
		avg := uint64(s.TotalLogicalSize) / uint64(s.CompletedFiles)
		lines = append(lines, statRow("  Avg file size", humanize.Bytes(avg), statValStyle))
	}

	lines = append(lines, "")

	// CHUNKS
	lines = append(lines, statSectionStyle.Render("  CHUNKS"))
	lines = append(lines, sep)
	lines = append(lines, statRow("  Unique chunks stored", humanize.Comma(s.UniqueChunks), statValStyle))
	lines = append(lines, statRow("  Total chunk references", humanize.Comma(s.TotalChunkRefs), statValStyle))
	if s.CompletedFiles > 0 {
		avgChunks := float64(s.TotalChunkRefs) / float64(s.CompletedFiles)
		lines = append(lines, statRow("  Avg chunks per file", fmt.Sprintf("%.1f", avgChunks), statValStyle))
	}
	if s.TotalChunkRefs > 0 && s.TotalLogicalSize > 0 {
		avgSize := uint64(s.TotalLogicalSize) / uint64(s.TotalChunkRefs)
		lines = append(lines, statRow("  Avg chunk size", humanize.Bytes(avgSize), statValStyle))
	}
	lines = append(lines, statRow("  Configured chunk size", humanize.Bytes(uint64(cnst.ChonkSize)), statDimStyle))

	deduped := s.TotalChunkRefs - s.UniqueChunks
	if deduped > 0 {
		lines = append(lines, statRow("  Duplicate refs avoided", humanize.Comma(deduped), statGoodStyle))
		if s.TotalChunkRefs > 0 {
			pct := float64(deduped) / float64(s.TotalChunkRefs) * 100
			lines = append(lines, statRow("  Dedup hit rate", fmt.Sprintf("%.1f%%", pct), statGoodStyle))
		}
	} else {
		lines = append(lines, statRow("  Duplicate refs avoided", "0 (no shared chunks yet)", statDimStyle))
	}
	if s.SharedChunks > 0 {
		lines = append(lines, statRow("  Chunks shared across files", humanize.Comma(s.SharedChunks), statGoodStyle))
	}

	lines = append(lines, "")

	// STRUCTURE
	lines = append(lines, statSectionStyle.Render("  STRUCTURE"))
	lines = append(lines, sep)
	lines = append(lines, statRow("  Partition files", humanize.Comma(s.TotalPartitions), statValStyle))
	lines = append(lines, statRow("  Indexed FS objects", humanize.Comma(s.TotalIndexedFiles), statValStyle))

	lines = append(lines, "")

	// STORAGE
	lines = append(lines, statSectionStyle.Render("  STORAGE"))
	lines = append(lines, sep)
	lines = append(lines, statRow("  Logical size", humanize.Bytes(uint64(s.TotalLogicalSize)), statValStyle))
	lines = append(lines, statRow("  On-disk size (blobs)", humanize.Bytes(uint64(s.OnDiskBytes)), statValStyle))
	if s.TotalLogicalSize > 0 && s.OnDiskBytes > 0 {
		if s.OnDiskBytes < s.TotalLogicalSize {
			saved := s.TotalLogicalSize - s.OnDiskBytes
			pct := float64(saved) / float64(s.TotalLogicalSize) * 100
			lines = append(lines, statRow("  Space saved", fmt.Sprintf("%s (%.1f%%)", humanize.Bytes(uint64(saved)), pct), statGoodStyle))
		} else {
			ratio := float64(s.OnDiskBytes) / float64(s.TotalLogicalSize)
			lines = append(lines, statRow("  Storage overhead", fmt.Sprintf("%.2fx (encrypted+compressed)", ratio), statDimStyle))
		}
	}

	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}
