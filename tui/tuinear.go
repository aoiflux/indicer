package tui

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const (
	nearIn  = "in"
	nearOut = "out"
)

const (
	nearFocusInput = iota
	nearFocusDeep
	nearFocusAdvanced
	nearFocusAnalyze
	nearFields
)

type nearItem struct {
	title, desc, mode string
}

func (i nearItem) FilterValue() string { return i.title }

type nearDelegate struct{ env *env }

func (nearDelegate) Height() int                         { return 2 }
func (nearDelegate) Spacing() int                        { return 0 }
func (nearDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }
func (d nearDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	i, ok := item.(nearItem)
	if !ok {
		return
	}
	fmt.Fprint(w, d.env.row(fmt.Sprintf("  %s\n  %s", i.title, i.desc), index == m.Index()))
}

type nearScreen struct {
	env    *env
	modes  list.Model
	mode   string // "" while choosing
	input  textinput.Model
	opts   NearOptions
	focus  int
	run    running
	asked  string
	status status
	out    Outcome
}

func newNearScreen(e *env) *nearScreen {
	modes := e.newList([]list.Item{
		nearItem{"A stored object", "Find what shares content with an object in the database", nearIn},
		nearItem{"A file outside the database", "Find what shares content with a file on disk", nearOut},
	}, nearDelegate{e}, "NeAR")
	return &nearScreen{env: e, modes: modes, input: e.newInput("")}
}

func (s *nearScreen) restyle() {
	s.modes.Styles = s.env.st.list
	s.input.SetStyles(s.env.st.textinput)
}

func (s *nearScreen) init() tea.Cmd { return nil }
func (s *nearScreen) job() int      { return s.run.id }
func (s *nearScreen) typing() bool  { return s.mode != "" && s.focus == nearFocusInput }

func (s *nearScreen) resize() {
	s.modes.SetWidth(max(32, s.env.width-12))
	s.modes.SetHeight(max(6, s.env.height-16))
	s.input.SetWidth(max(24, s.env.width-14))
}

// escape goes back to choosing the mode, unless the analysis runs.
func (s *nearScreen) escape() bool {
	if s.mode == "" || s.run.active() {
		return false
	}
	s.mode, s.out, s.status = "", Outcome{}, status{}
	s.input.Blur()
	return true
}

func (s *nearScreen) setFocus(f int) {
	s.focus = f
	if f == nearFocusInput {
		s.input.Focus()
	} else {
		s.input.Blur()
	}
}

func (s *nearScreen) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case jobDoneMsg:
		s.run = running{}
		s.out, _ = msg.result.(Outcome)
		switch {
		case msg.err != nil:
			s.status = errStatus(msg.err)
		case s.out.Warning:
			s.status = status{statusWarn, "Analysed, with warnings: see below"}
		default:
			s.status = status{statusOK, "Analysed"}
		}
		return nil
	case tea.KeyPressMsg:
		if s.mode == "" {
			return s.modeKey(msg)
		}
		return s.key(msg)
	}
	if s.mode != "" && s.focus == nearFocusInput {
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return cmd
	}
	return nil
}

func (s *nearScreen) modeKey(msg tea.KeyPressMsg) tea.Cmd {
	if msg.String() == "enter" {
		// A checked assertion: an empty selection is not an item (TUI-4).
		item, ok := s.modes.SelectedItem().(nearItem)
		if !ok {
			return nil
		}
		s.mode = item.mode
		s.input.SetValue("")
		if s.mode == nearIn {
			s.input.Placeholder = "ID or hash of a stored object…"
		} else {
			s.input.Placeholder = "Path of a file…"
		}
		s.setFocus(nearFocusInput)
		return textinput.Blink
	}
	var cmd tea.Cmd
	s.modes, cmd = s.modes.Update(msg)
	return cmd
}

func (s *nearScreen) key(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "tab", "down":
		s.setFocus((s.focus + 1) % nearFields)
		return nil
	case "shift+tab", "up":
		s.setFocus((s.focus + nearFields - 1) % nearFields)
		return nil
	case "enter":
		switch s.focus {
		case nearFocusInput, nearFocusAnalyze:
			return s.start()
		default:
			s.toggle()
		}
		return nil
	case "space":
		if s.toggle() {
			return nil
		}
	}
	if s.focus == nearFocusInput {
		// Every key typed here is text, d included (TUI-4).
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return cmd
	}
	return nil
}

// toggle flips the checkbox that has the focus, and reports whether one had.
func (s *nearScreen) toggle() bool {
	switch s.focus {
	case nearFocusDeep:
		s.opts.Deep = !s.opts.Deep
	case nearFocusAdvanced:
		s.opts.Advanced = !s.opts.Advanced
	default:
		return false
	}
	return true
}

func (s *nearScreen) start() tea.Cmd {
	if s.run.active() {
		return nil
	}
	input := strings.TrimSpace(s.input.Value())
	if input == "" {
		s.status = status{statusError, "Error: enter what to analyse"}
		return nil
	}
	var name string
	var near func(context.Context, string, NearOptions) (Outcome, error)
	switch s.mode {
	case nearIn:
		name, near = "near "+truncate(input, 12), s.env.actions.NearIn
	case nearOut:
		name, near = "near "+filepath.Base(input), s.env.actions.NearOut
	default:
		return nil
	}
	if near == nil {
		s.status = status{statusError, "Error: NeAR is not available"}
		return nil
	}
	opts := s.opts
	fn := func(ctx context.Context) (any, error) { return near(ctx, input, opts) }
	id, cmd, err := s.env.run(name, sharedJob, fn)
	if err != nil {
		s.status = errStatus(err)
		return nil
	}
	s.run = running{id: id, since: time.Now()}
	s.asked = input
	s.out = Outcome{}
	s.status = status{}
	return cmd
}

func (s *nearScreen) view() string {
	e, st := s.env, s.env.st
	title := st.title.Render("NeAR: find stored objects that share content")
	if s.mode == "" {
		return e.frame(joinLines(
			title,
			s.modes.View(),
			st.help.Render("↑/↓: Choose | Enter: Select | esc: Back"),
		))
	}
	label := "Stored object (ID or hash):"
	if s.mode == nearOut {
		label = "File:"
	}
	if s.focus == nearFocusInput {
		label = st.selected.Render(label)
	}
	option := func(f int, on bool, label string) string {
		box := checkbox(on, label)
		if s.focus == f {
			box = st.selected.Render(box)
		}
		return box
	}
	parts := []string{
		title,
		st.input.Render(label + "\n" + s.input.View()),
		option(nearFocusDeep, s.opts.Deep, "Deep: also match similar chunks (needs chunk signatures; slower)") + "\n" +
			option(nearFocusAdvanced, s.opts.Advanced, "Advanced deep: re-rank the closest candidates by whole-file similarity (slower)"),
	}
	stat := s.status.render(st)
	if s.run.active() {
		stat = st.info.Render(fmt.Sprintf("Analysing %s… %s", clean(s.asked), since(s.run.since)))
	}
	parts = append(parts,
		lipgloss.JoinHorizontal(lipgloss.Left, button(st, "Analyse", s.focus == nearFocusAnalyze)),
		st.dim.Render("The report replaces near_report.json in the working directory."),
		stat,
		outcomeLines(s.out, e.width-8),
		st.help.Render("Tab/↑/↓: Move | Enter: Select | Space: Toggle | esc: Choose again | ctrl+x: Cancel the analysis"),
	)
	return e.frame(joinLines(parts...))
}
