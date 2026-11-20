package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
)

// microValueWidth is the most of an artefact's value shown.
const microValueWidth = 100

type microScreen struct {
	env      *env
	run      running
	loaded   bool
	micro    MicroView
	status   status
	viewport viewport.Model
}

func newMicroScreen(e *env) *microScreen {
	vp := viewport.New(viewport.WithWidth(76), viewport.WithHeight(20))
	vp.SoftWrap = false
	return &microScreen{env: e, viewport: vp}
}

func (s *microScreen) typing() bool { return false }
func (s *microScreen) job() int     { return s.run.id }

func (s *microScreen) resize() {
	s.viewport.SetWidth(max(50, s.env.width-8))
	s.viewport.SetHeight(max(6, s.env.height-11))
}

func (s *microScreen) init() tea.Cmd {
	read := s.env.actions.Micro
	if read == nil {
		s.status = status{statusError, "Error: micro-artefacts are not available"}
		return nil
	}
	id, cmd, err := s.env.run("micro-artefacts", loaderJob, func(ctx context.Context) (any, error) {
		return read(ctx)
	})
	if err != nil {
		s.status = errStatus(err)
		return nil
	}
	s.run = running{id: id, since: time.Now()}
	return cmd
}

func (s *microScreen) update(msg tea.Msg) tea.Cmd {
	if msg, ok := msg.(jobDoneMsg); ok {
		s.run = running{}
		if msg.err != nil {
			s.status = errStatus(msg.err)
			return nil
		}
		s.micro, _ = msg.result.(MicroView)
		s.loaded = true
		s.viewport.SetContent(s.render())
		return nil
	}
	var cmd tea.Cmd
	s.viewport, cmd = s.viewport.Update(msg)
	return cmd
}

func (s *microScreen) view() string {
	e, st := s.env, s.env.st
	var body, summary string
	switch {
	case s.run.active():
		body = st.info.Render(fmt.Sprintf("Reading the graph… %s (ctrl+x cancels)", since(s.run.since)))
	case !s.loaded:
		body = s.status.render(st)
	case s.micro.Files == 0:
		body = st.dim.Render("No file has micro-artefacts. Run `dues micro extract` first.")
	default:
		summary = st.dim.Render(fmt.Sprintf("%d file(s) with %d artefact(s)", s.micro.Files, s.micro.Artefacts))
		if s.micro.Shown < s.micro.Files {
			summary = st.dim.Render(fmt.Sprintf("The %d files with the most artefacts, of %d with %d artefact(s); `dues micro extract` writes the full view.",
				s.micro.Shown, s.micro.Files, s.micro.Artefacts))
		}
		body = s.viewport.View()
	}
	return e.frame(joinLines(
		st.title.Render("Micro-artefacts"),
		summary,
		body,
		st.help.Render("↑/↓/PgUp/PgDn: Scroll | esc/q: Back"),
	))
}

// render draws the tree. Every name and value comes from the evidence and is
// made safe to draw (TUI-5).
func (s *microScreen) render() string {
	t := s.micro.Tree
	if t == nil {
		return ""
	}
	var b strings.Builder
	for _, ev := range t.EvidenceFiles {
		fmt.Fprintf(&b, "evidence: %s\n", clean(fallback(ev.Name, ev.ID)))
		for _, part := range ev.Partitions {
			fmt.Fprintf(&b, "  partition: %s\n", clean(fallback(part.Name, part.ID)))
			for _, f := range part.Files {
				name := fallback(f.Path, fallback(f.Name, f.ID))
				count := max(f.ArtefactCount, len(f.Artefacts))
				fmt.Fprintf(&b, "    file: %s (%d bytes, %d artefacts)\n", clean(name), f.Size, count)
				for _, a := range f.Artefacts {
					fmt.Fprintf(&b, "      - %s: %s\n", clean(a.Kind), truncate(clean(a.Value), microValueWidth))
				}
				if more := count - len(f.Artefacts); more > 0 {
					fmt.Fprintf(&b, "      … and %d more\n", more)
				}
			}
		}
		b.WriteString("\n")
	}
	return b.String()
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
