package tui

import (
	"context"
	"fmt"
	"time"

	"indicer/lib/store"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/dustin/go-humanize"
)

type statsScreen struct {
	env      *env
	run      running
	stats    *store.DBStats
	status   status
	viewport viewport.Model
}

func newStatsScreen(e *env) *statsScreen {
	vp := viewport.New(viewport.WithWidth(76), viewport.WithHeight(20))
	vp.SoftWrap = true
	return &statsScreen{env: e, viewport: vp}
}

func (s *statsScreen) typing() bool { return false }
func (s *statsScreen) job() int     { return s.run.id }

func (s *statsScreen) resize() {
	s.viewport.SetWidth(max(50, s.env.width-8))
	s.viewport.SetHeight(max(6, s.env.height-10))
}

func (s *statsScreen) init() tea.Cmd {
	gather := s.env.actions.Stats
	if gather == nil {
		s.status = status{statusError, "Error: statistics are not available"}
		return nil
	}
	id, cmd, err := s.env.run("statistics", loaderJob, func(ctx context.Context) (any, error) {
		return gather(ctx)
	})
	if err != nil {
		s.status = errStatus(err)
		return nil
	}
	s.run = running{id: id, since: time.Now()}
	return cmd
}

func (s *statsScreen) update(msg tea.Msg) tea.Cmd {
	if msg, ok := msg.(jobDoneMsg); ok {
		s.run = running{}
		if msg.err != nil {
			s.status = errStatus(msg.err)
			return nil
		}
		s.stats, _ = msg.result.(*store.DBStats)
		if s.stats != nil {
			s.viewport.SetContent(s.render())
		}
		return nil
	}
	var cmd tea.Cmd
	s.viewport, cmd = s.viewport.Update(msg)
	return cmd
}

func (s *statsScreen) view() string {
	e := s.env
	var body string
	switch {
	case s.run.active():
		body = InfoStyle.Render(fmt.Sprintf("Gathering statistics… %s (ctrl+x cancels)", since(s.run.since)))
	case s.stats == nil:
		body = s.status.render()
	default:
		body = s.viewport.View() + "\n" + DimStyle.Render(fmt.Sprintf(" %d%%", int(s.viewport.ScrollPercent()*100)))
	}
	return frame(e.width, e.height, joinLines(
		TitleStyle.Render("📊 Database statistics"),
		body,
		HelpStyle.Render("↑/↓/PgUp/PgDn: Scroll | esc/q: Back"),
	))
}

var (
	statSectionStyle = lipgloss.NewStyle().Bold(true).Foreground(primaryColor)
	statLabelStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Width(32)
	statValStyle     = lipgloss.NewStyle().Foreground(accentColor).Bold(true)
	statGoodStyle    = lipgloss.NewStyle().Foreground(successColor).Bold(true)
	statBadStyle     = lipgloss.NewStyle().Foreground(errorColor).Bold(true)
)

func statRow(label, value string, valStyle lipgloss.Style) string {
	return statLabelStyle.Render(label+":") + " " + valStyle.Render(value)
}

func (s *statsScreen) render() string {
	st, info := s.stats, s.env.info
	sep := DimStyle.Render("  ─────────────────────────────────────────────")
	pct := func(n, of int64) string {
		return fmt.Sprintf("%s (%.1f%%)", humanize.Comma(n), float64(n)/float64(of)*100)
	}

	lines := []string{statSectionStyle.Render("  EVIDENCE"), sep,
		statRow("  Evidence records", humanize.Comma(st.TotalFiles), statValStyle),
		statRow("  Stored", humanize.Comma(st.CompletedFiles), statGoodStyle),
	}
	if inc := st.TotalFiles - st.CompletedFiles; inc > 0 {
		lines = append(lines, statRow("  Not stored (pending, failed)", humanize.Comma(inc), statBadStyle))
	}
	lines = append(lines, statRow("  Logical size", humanize.IBytes(uint64(st.TotalLogicalSize)), statValStyle))
	if st.CompletedFiles > 0 {
		lines = append(lines, statRow("  Average size", humanize.IBytes(uint64(st.TotalLogicalSize/st.CompletedFiles)), statValStyle))
	}

	lines = append(lines, "", statSectionStyle.Render("  CHUNKS"), sep,
		statRow("  Chunk size", fmt.Sprintf("%d KiB", info.ChunkKiB), DimStyle),
		statRow("  Chunk hash", clean(info.ChunkHash), DimStyle),
		statRow("  Unique chunks stored", humanize.Comma(st.UniqueChunks), statValStyle),
		statRow("  Chunk references", humanize.Comma(st.TotalChunkRefs), statValStyle),
	)
	if deduped := st.TotalChunkRefs - st.UniqueChunks; deduped > 0 && st.TotalChunkRefs > 0 {
		lines = append(lines,
			statRow("  References to a stored chunk", humanize.Comma(deduped), statGoodStyle),
			statRow("  Deduplication rate", fmt.Sprintf("%.1f%%", float64(deduped)/float64(st.TotalChunkRefs)*100), statGoodStyle))
	}
	if st.SharedChunks > 0 {
		lines = append(lines, statRow("  Chunks shared by objects", humanize.Comma(st.SharedChunks), statGoodStyle))
	}

	lines = append(lines, "", statSectionStyle.Render("  INDEX"), sep,
		statRow("  Partitions", humanize.Comma(st.TotalPartitions), statValStyle),
		statRow("  Indexed files", humanize.Comma(st.TotalIndexedFiles), statValStyle),
	)
	if st.TotalIndexedFiles > 0 {
		lines = append(lines,
			statRow("  Deleted", pct(st.DeletedIndexedFiles, st.TotalIndexedFiles), statValStyle),
			statRow("  Fragmented", pct(st.FragmentedIndexedFiles, st.TotalIndexedFiles), statValStyle))
	}

	lines = append(lines, "", statSectionStyle.Render("  STORAGE"), sep,
		statRow("  On disk (blobs and containers)", humanize.IBytes(uint64(st.OnDiskBytes)), statValStyle))
	if st.TotalLogicalSize > 0 && st.OnDiskBytes > 0 {
		if st.OnDiskBytes < st.TotalLogicalSize {
			saved := st.TotalLogicalSize - st.OnDiskBytes
			lines = append(lines, statRow("  Space saved", fmt.Sprintf("%s (%.1f%%)", humanize.IBytes(uint64(saved)), float64(saved)/float64(st.TotalLogicalSize)*100), statGoodStyle))
		} else {
			lines = append(lines, statRow("  On disk / logical", fmt.Sprintf("%.2fx", float64(st.OnDiskBytes)/float64(st.TotalLogicalSize)), DimStyle))
		}
	}
	if !info.Encrypted {
		lines = append(lines, statRow("  Encryption", "none", statBadStyle))
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}
