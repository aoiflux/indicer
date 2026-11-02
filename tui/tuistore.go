package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const (
	storeFocusPath = iota
	storeFocusNoIndex
	storeFocusStore
	storeFocusCancel
	storeFields
)

type storeScreen struct {
	env     *env
	path    textinput.Model
	noIndex bool
	focus   int
	run     running
	status  status
	out     Outcome
	another prompt // after a store: store another?
}

func newStoreScreen(e *env) *storeScreen {
	in := e.newInput("Path of a file or folder…")
	in.Focus()
	return &storeScreen{env: e, path: in}
}

func (s *storeScreen) restyle() { s.path.SetStyles(s.env.st.textinput) }

func (s *storeScreen) init() tea.Cmd { return textinput.Blink }
func (s *storeScreen) resize()       { s.path.SetWidth(max(24, s.env.width-14)) }
func (s *storeScreen) job() int      { return s.run.id }
func (s *storeScreen) typing() bool  { return s.focus == storeFocusPath && !s.another.open }

func (s *storeScreen) setFocus(f int) {
	s.focus = f
	if f == storeFocusPath {
		s.path.Focus()
	} else {
		s.path.Blur()
	}
}

func (s *storeScreen) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case jobDoneMsg:
		s.run = running{}
		s.out, _ = msg.result.(Outcome)
		switch {
		case msg.err != nil:
			s.status = errStatus(msg.err)
			return nil
		case s.out.Warning:
			s.status = status{statusWarn, "Stored, with warnings: see below"}
		default:
			s.status = status{statusOK, "Stored"}
		}
		s.setFocus(-1)
		s.another.ask("Store another file or folder?", "Yes", "No", true)
		return nil
	case tea.KeyPressMsg:
		if s.another.open {
			decided, yes := s.another.key(msg)
			switch {
			case !decided:
				return nil
			case yes:
				s.path.SetValue("")
				s.out = Outcome{}
				s.status = infoStatus("Ready to store another file or folder")
				s.setFocus(storeFocusPath)
				return nil
			}
			return back
		}
		return s.key(msg)
	}
	if s.focus == storeFocusPath {
		var cmd tea.Cmd
		s.path, cmd = s.path.Update(msg)
		return cmd
	}
	return nil
}

func (s *storeScreen) key(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "tab", "down":
		s.setFocus((s.focus + 1) % storeFields)
		return nil
	case "shift+tab", "up":
		s.setFocus((s.focus + storeFields - 1) % storeFields)
		return nil
	case "enter":
		switch s.focus {
		case storeFocusPath, storeFocusStore:
			return s.start()
		case storeFocusNoIndex:
			s.noIndex = !s.noIndex
		case storeFocusCancel:
			// Cancel goes back to the menu; it used to quit (TUI-4).
			return back
		}
		return nil
	case "space":
		if s.focus == storeFocusNoIndex {
			s.noIndex = !s.noIndex
			return nil
		}
	}
	if s.focus == storeFocusPath {
		var cmd tea.Cmd
		s.path, cmd = s.path.Update(msg)
		return cmd
	}
	return nil
}

func (s *storeScreen) start() tea.Cmd {
	if s.run.active() {
		return nil
	}
	path := strings.TrimSpace(s.path.Value())
	if path == "" {
		s.status = status{statusError, "Error: enter the path of a file or folder"}
		return nil
	}
	store := s.env.actions.Store
	if store == nil {
		s.status = status{statusError, "Error: storing is not available"}
		return nil
	}
	opts := StoreOptions{NoIndex: s.noIndex}
	id, cmd, err := s.env.run("store "+filepath.Base(path), sharedJob, func(ctx context.Context) (any, error) {
		return store(ctx, path, opts)
	})
	if err != nil {
		s.status = errStatus(err)
		return nil
	}
	s.run = running{id: id, since: time.Now()}
	s.out = Outcome{}
	s.status = status{}
	return cmd
}

func (s *storeScreen) view() string {
	e, st := s.env, s.env.st
	label := "File or folder:"
	if s.focus == storeFocusPath {
		label = st.selected.Render(label)
	}
	option := checkbox(s.noIndex, "Skip the index: store the bytes only")
	if s.focus == storeFocusNoIndex {
		option = st.selected.Render(option)
	}
	layout := st.dim.Render(fmt.Sprintf("Chunks of %d KiB, hashed with %s: the database's own layout, fixed when it was created.",
		e.info.ChunkKiB, clean(e.info.ChunkHash)))
	buttons := lipgloss.JoinHorizontal(lipgloss.Left,
		button(st, "Store", s.focus == storeFocusStore), button(st, "Cancel", s.focus == storeFocusCancel))
	stat := s.status.render(st)
	if s.run.active() {
		stat = st.info.Render(fmt.Sprintf("Storing %s… %s", clean(strings.TrimSpace(s.path.Value())), since(s.run.since)))
	}
	help := "Tab/↑/↓: Move | Enter: Select | Space: Toggle | esc: Back (a store keeps running) | ctrl+x: Cancel the store"
	if s.another.open {
		help = ""
	}
	return e.frame(joinLines(
		st.title.Render("Store evidence"),
		st.input.Render(label+"\n"+s.path.View()),
		option+"\n"+layout,
		buttons,
		stat,
		outcomeLines(s.out, e.width-8),
		s.another.view(st),
		st.help.Render(help),
	))
}
