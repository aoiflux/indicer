package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
	"github.com/dustin/go-humanize"
)

// listLimit is the most evidence the list screen loads; `dues list` shows
// every one (TUI-5).
const listLimit = 5000

// writeClipboard is the clipboard; tests replace it.
var writeClipboard = clipboard.WriteAll

// copiedMsg reports a clipboard write.
type copiedMsg struct {
	text string
	err  error
}

// copyCmd writes text to the clipboard. It runs as a command: Update must
// not wait for the clipboard (TUI-5).
func copyCmd(text string) tea.Cmd {
	return func() tea.Msg { return copiedMsg{text: text, err: writeClipboard(text)} }
}

type listScreen struct {
	env      *env
	run      running
	loading  bool
	loaded   bool
	list     EvidenceList
	sel      int
	status   status
	target   string // the path the selected evidence is restored to
	restored string // the ID being restored
	replace  prompt
}

func newListScreen(e *env) *listScreen {
	return &listScreen{env: e, loading: true}
}

func (s *listScreen) resize()      {}
func (s *listScreen) typing() bool { return false }
func (s *listScreen) job() int     { return s.run.id }

func (s *listScreen) escape() bool {
	if s.replace.open {
		s.replace.open = false
		s.status = infoStatus("Not restored: " + clean(s.target) + " exists")
		return true
	}
	return false
}

func (s *listScreen) init() tea.Cmd {
	load := s.env.actions.List
	if load == nil {
		s.loading = false
		s.status = status{statusError, "Error: listing is not available"}
		return nil
	}
	id, cmd, err := s.env.run("list", loaderJob, func(ctx context.Context) (any, error) {
		return load(ctx, listLimit)
	})
	if err != nil {
		s.loading = false
		s.status = errStatus(err)
		return nil
	}
	s.run = running{id: id, since: time.Now()}
	return cmd
}

func (s *listScreen) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case jobDoneMsg:
		s.run = running{}
		if s.loading {
			s.loading = false
			if msg.err != nil {
				s.status = errStatus(msg.err)
				return nil
			}
			s.list, _ = msg.result.(EvidenceList)
			s.loaded = true
			return nil
		}
		return s.restoreDone(msg)
	case copiedMsg:
		if msg.err != nil {
			s.status = status{statusError, "Error: copying to the clipboard: " + errorText(msg.err)}
		} else {
			s.status = status{statusOK, "Copied " + clean(msg.text)}
		}
		return nil
	case tea.KeyPressMsg:
		if s.replace.open {
			if decided, yes := s.replace.key(msg); decided {
				if yes {
					return s.restore(true)
				}
				s.status = infoStatus("Not restored: " + clean(s.target) + " exists")
			}
			return nil
		}
		return s.key(msg)
	}
	return nil
}

func (s *listScreen) key(msg tea.KeyPressMsg) tea.Cmd {
	n := len(s.list.Rows)
	if n == 0 {
		return nil
	}
	page := max(1, s.rows()-1)
	switch msg.String() {
	case "up", "k":
		s.sel = max(0, s.sel-1)
	case "down", "j":
		s.sel = min(n-1, s.sel+1)
	case "pgup":
		s.sel = max(0, s.sel-page)
	case "pgdown":
		s.sel = min(n-1, s.sel+page)
	case "home", "g":
		s.sel = 0
	case "end", "G":
		s.sel = n - 1
	case "c":
		return copyCmd(s.list.Rows[s.sel].ID)
	case "r", "enter":
		return s.restore(false)
	}
	return nil
}

// restore restores the selected evidence beside the working directory.
func (s *listScreen) restore(replace bool) tea.Cmd {
	if s.run.active() || len(s.list.Rows) == 0 {
		return nil
	}
	restore := s.env.actions.Restore
	if restore == nil {
		s.status = status{statusError, "Error: restoring is not available"}
		return nil
	}
	id := s.list.Rows[s.sel].ID
	target := defaultRestorePath(id)
	jobID, cmd, err := s.env.run("restore "+truncate(id, 12), sharedJob, func(ctx context.Context) (any, error) {
		return restore(ctx, id, target, replace)
	})
	if err != nil {
		s.status = errStatus(err)
		return nil
	}
	s.run = running{id: jobID, since: time.Now()}
	s.target, s.restored = target, id
	s.status = infoStatus("Restoring " + clean(id) + " to " + clean(target) + "…")
	return cmd
}

func (s *listScreen) restoreDone(msg jobDoneMsg) tea.Cmd {
	out, _ := msg.result.(Outcome)
	switch {
	case errors.Is(msg.err, ErrTargetExists):
		s.status = status{}
		s.replace.ask(clean(s.target)+" exists. Replace it?", "Yes, replace it", "No", false)
	case msg.err != nil:
		s.status = errStatus(msg.err)
	case out.Warning:
		s.status = status{statusWarn, "Restored to " + clean(s.target) + ", with warnings: " + strings.Join(cleanAll(out.Lines), "; ")}
	default:
		s.status = status{statusOK, "Restored to " + clean(s.target)}
	}
	return nil
}

// rows is how many evidence rows fit on the screen.
func (s *listScreen) rows() int {
	return max(3, s.env.height-14)
}

func (s *listScreen) view() string {
	e, st := s.env, s.env.st
	title := st.title.Render("Stored evidence")
	switch {
	case s.loading:
		return e.frame(joinLines(title, st.info.Render("Loading…")))
	case !s.loaded:
		return e.frame(joinLines(title, s.status.render(st), st.help.Render("esc: Back")))
	case len(s.list.Rows) == 0:
		return e.frame(joinLines(title, st.info.Render("No evidence is stored."), st.help.Render("esc: Back")))
	}

	count := fmt.Sprintf("%s evidence", humanize.Comma(int64(s.list.Total)))
	if s.list.Total > len(s.list.Rows) {
		count = fmt.Sprintf("The first %s of %s evidence; `dues list` lists them all.", humanize.Comma(int64(len(s.list.Rows))), humanize.Comma(int64(s.list.Total)))
	}
	nameWidth := max(10, e.width-24-10-8-10-12)
	header := fmt.Sprintf("%-24s %10s %-8s %-10s %s", "ID", "Size", "Format", "Index", "Name")
	rows := []string{st.dim.Render(header)}
	from, to := window(len(s.list.Rows), s.sel, s.rows())
	for i := from; i < to; i++ {
		r := s.list.Rows[i]
		line := fmt.Sprintf("%-24s %10s %-8s %-10s %s",
			truncate(clean(r.ID), 24), humanize.IBytes(uint64(max(0, r.Size))), truncate(clean(r.Format), 8),
			truncate(clean(r.Index), 10), truncate(clean(r.Name), nameWidth))
		if i == s.sel {
			line = st.selected.Render(line)
		} else {
			line = st.normal.Render(line)
		}
		rows = append(rows, line)
	}
	position := st.dim.Render(fmt.Sprintf("%d of %d", s.sel+1, len(s.list.Rows)))
	help := "↑/↓/PgUp/PgDn/Home/End: Move | Enter/r: Restore to restored_<ID>.bin | c: Copy the ID | esc: Back"
	if s.replace.open {
		help = ""
	}
	return e.frame(joinLines(
		title,
		count,
		strings.Join(rows, "\n")+"\n"+position,
		s.status.render(st),
		s.replace.view(st),
		st.help.Render(help),
	))
}

func cleanAll(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = clean(l)
	}
	return out
}

// defaultRestorePath is where the list restores an evidence: a name in the
// working directory made from its ID.
func defaultRestorePath(id string) string {
	name := id
	if len(name) > 16 {
		name = name[:16]
	}
	return "restored_" + strings.NewReplacer("/", "_", "+", "-", "=", "").Replace(name) + ".bin"
}
