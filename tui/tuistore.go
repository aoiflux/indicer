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

// The store screen's options, in the order they are shown.
const (
	optNoIndex = iota
	optRevRel
	optSimhash
	optFTS
	optEnrich
	storeOptionCount
)

var storeOptionLabels = [storeOptionCount]string{
	"Skip the index: store the bytes only",
	"Reverse relations: needed by NeAR (slower store)",
	"Chunk signatures: needed by deep NeAR (slower store)",
	"Full-text index: for `dues search --fts`",
	"Graph enrichment: entropy and the file hierarchy in the graph",
}

// The fields focus moves through: the path, each option, the buttons.
const (
	storeFocusPath   = 0
	storeFocusStore  = storeOptionCount + 1
	storeFocusCancel = storeOptionCount + 2
	storeFields      = storeOptionCount + 3
)

// storeFocusOption is the focus of option i.
func storeFocusOption(i int) int { return i + 1 }

type storeScreen struct {
	env     *env
	path    textinput.Model
	opts    [storeOptionCount]bool
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

// option is the option the focus is on, or -1.
func (s *storeScreen) option() int {
	if i := s.focus - 1; i >= 0 && i < storeOptionCount {
		return i
	}
	return -1
}

// available reports whether option i can be chosen: the full-text index and
// enrichment read the index, so they need it.
func (s *storeScreen) available(i int) bool {
	return !s.opts[optNoIndex] || (i != optFTS && i != optEnrich)
}

func (s *storeScreen) setFocus(f int) {
	s.focus = f
	if f == storeFocusPath {
		s.path.Focus()
	} else {
		s.path.Blur()
	}
}

// move moves the focus by step, past options that cannot be chosen.
func (s *storeScreen) move(step int) {
	f := s.focus
	for {
		f = (f + step + storeFields) % storeFields
		if i := f - 1; i < 0 || i >= storeOptionCount || s.available(i) {
			break
		}
	}
	s.setFocus(f)
}

// toggle flips the option the focus is on, and reports whether it was on
// one. Skipping the index turns off what needs it.
func (s *storeScreen) toggle() bool {
	i := s.option()
	if i < 0 {
		return false
	}
	s.opts[i] = !s.opts[i]
	if i == optNoIndex && s.opts[i] {
		s.opts[optFTS], s.opts[optEnrich] = false, false
	}
	return true
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
		s.move(1)
		return nil
	case "shift+tab", "up":
		s.move(-1)
		return nil
	case "enter":
		switch s.focus {
		case storeFocusPath, storeFocusStore:
			return s.start()
		case storeFocusCancel:
			// Cancel goes back to the menu; it used to quit (TUI-4).
			return back
		}
		s.toggle()
		return nil
	case "space":
		if s.toggle() {
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
	o := s.opts
	opts := StoreOptions{
		NoIndex: o[optNoIndex], RevRel: o[optRevRel], Simhash: o[optSimhash],
		FTS: o[optFTS] && !o[optNoIndex], Enrich: o[optEnrich] && !o[optNoIndex],
	}
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
	options := make([]string, 0, storeOptionCount+1)
	for i, text := range storeOptionLabels {
		box := checkbox(s.opts[i], text)
		switch {
		case !s.available(i):
			box = st.dim.Render(box + " (needs the index)")
		case s.focus == storeFocusOption(i):
			box = st.selected.Render(box)
		}
		options = append(options, box)
	}
	options = append(options, st.dim.Render(fmt.Sprintf("Chunks of %d KiB, hashed with %s: the database's own layout, fixed when it was created.",
		e.info.ChunkKiB, clean(e.info.ChunkHash))))
	buttons := lipgloss.JoinHorizontal(lipgloss.Left,
		button(st, "Store", s.focus == storeFocusStore), button(st, "Cancel", s.focus == storeFocusCancel))
	stat := s.status.render(st)
	if s.run.active() {
		stat = st.info.Render(fmt.Sprintf("Storing %s… %s", clean(strings.TrimSpace(s.path.Value())), since(s.run.since)))
	}
	help := "Tab/↑/↓: Move | Enter: Select | Space: Toggle | esc: Back (a store keeps running) | ctrl+x: Cancel the store"
	optionsView := strings.Join(options, "\n")
	if s.another.open {
		// The question and the report need the room; the options apply to
		// the next store, which the question asks about.
		help, optionsView = "", ""
	}
	return e.frame(joinLines(
		st.title.Render("Store evidence"),
		st.input.Render(label+"\n"+s.path.View()),
		optionsView,
		buttons,
		stat,
		outcomeLines(s.out, e.width-8),
		s.another.view(st),
		st.help.Render(help),
	))
}
