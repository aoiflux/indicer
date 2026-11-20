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
		s.restyle()
		return nil
	}
	var cmd tea.Cmd
	s.viewport, cmd = s.viewport.Update(msg)
	return cmd
}

// restyle draws the statistics in the current styles.
func (s *statsScreen) restyle() {
	if s.stats != nil {
		s.viewport.SetContent(s.render())
	}
}

func (s *statsScreen) view() string {
	e, st := s.env, s.env.st
	var body string
	switch {
	case s.run.active():
		body = st.info.Render(fmt.Sprintf("Gathering statistics… %s (ctrl+x cancels)", since(s.run.since)))
	case s.stats == nil:
		body = s.status.render(st)
	default:
		body = s.viewport.View() + "\n" + st.dim.Render(fmt.Sprintf(" %d%%", int(s.viewport.ScrollPercent()*100)))
	}
	return e.frame(joinLines(
		st.title.Render("Database statistics"),
		body,
		st.help.Render("↑/↓/PgUp/PgDn: Scroll | esc/q: Back"),
	))
}

func (s *statsScreen) render() string {
	db, info, st := s.stats, s.env.info, s.env.st
	row := func(label, value string, val lipgloss.Style) string {
		return st.statLabel.Render(label+":") + " " + val.Render(value)
	}
	sep := st.dim.Render("  ─────────────────────────────────────────────")
	pct := func(n, of int64) string {
		return fmt.Sprintf("%s (%.1f%%)", humanize.Comma(n), float64(n)/float64(of)*100)
	}

	lines := []string{st.statSection.Render("  EVIDENCE"), sep,
		row("  Evidence records", humanize.Comma(db.TotalFiles), st.statValue),
		row("  Stored", humanize.Comma(db.CompletedFiles), st.statGood),
	}
	if inc := db.TotalFiles - db.CompletedFiles; inc > 0 {
		lines = append(lines, row("  Not stored (pending, failed)", humanize.Comma(inc), st.statBad))
	}
	lines = append(lines, row("  Logical size", humanize.IBytes(uint64(db.TotalLogicalSize)), st.statValue))
	if db.CompletedFiles > 0 {
		lines = append(lines, row("  Average size", humanize.IBytes(uint64(db.TotalLogicalSize/db.CompletedFiles)), st.statValue))
	}

	lines = append(lines, "", st.statSection.Render("  CHUNKS"), sep,
		row("  Chunk size", fmt.Sprintf("%d KiB", info.ChunkKiB), st.dim),
		row("  Chunk hash", clean(info.ChunkHash), st.dim),
		row("  Unique chunks stored", humanize.Comma(db.UniqueChunks), st.statValue),
		row("  Chunk references", humanize.Comma(db.TotalChunkRefs), st.statValue),
	)
	if deduped := db.TotalChunkRefs - db.UniqueChunks; deduped > 0 && db.TotalChunkRefs > 0 {
		lines = append(lines,
			row("  References to a stored chunk", humanize.Comma(deduped), st.statGood),
			row("  Deduplication rate", fmt.Sprintf("%.1f%%", float64(deduped)/float64(db.TotalChunkRefs)*100), st.statGood))
	}
	if db.SharedChunks > 0 {
		lines = append(lines, row("  Chunks shared by objects", humanize.Comma(db.SharedChunks), st.statGood))
	}

	lines = append(lines, "", st.statSection.Render("  INDEX"), sep,
		row("  Partitions", humanize.Comma(db.TotalPartitions), st.statValue),
		row("  Indexed files", humanize.Comma(db.TotalIndexedFiles), st.statValue),
	)
	if db.TotalIndexedFiles > 0 {
		lines = append(lines,
			row("  Deleted", pct(db.DeletedIndexedFiles, db.TotalIndexedFiles), st.statValue),
			row("  Fragmented", pct(db.FragmentedIndexedFiles, db.TotalIndexedFiles), st.statValue))
	}

	lines = append(lines, "", st.statSection.Render("  STORAGE"), sep,
		row("  On disk (blobs and containers)", humanize.IBytes(uint64(db.OnDiskBytes)), st.statValue))
	if db.TotalLogicalSize > 0 && db.OnDiskBytes > 0 {
		if db.OnDiskBytes < db.TotalLogicalSize {
			saved := db.TotalLogicalSize - db.OnDiskBytes
			lines = append(lines, row("  Space saved", fmt.Sprintf("%s (%.1f%%)", humanize.IBytes(uint64(saved)), float64(saved)/float64(db.TotalLogicalSize)*100), st.statGood))
		} else {
			lines = append(lines, row("  On disk / logical", fmt.Sprintf("%.2fx", float64(db.OnDiskBytes)/float64(db.TotalLogicalSize)), st.dim))
		}
	}
	if !info.Encrypted {
		lines = append(lines, row("  Encryption", "none", st.statBad))
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}
