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
	nearFocusDeep  // "in" only
	nearFocusAnalyze
	nearFields
)

type nearItem struct {
	title, desc, mode string
}

func (i nearItem) FilterValue() string { return i.title }

type nearDelegate struct{}

func (nearDelegate) Height() int                         { return 2 }
func (nearDelegate) Spacing() int                        { return 0 }
func (nearDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }
func (nearDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	i, ok := item.(nearItem)
	if !ok {
		return
	}
	str := fmt.Sprintf("  %s\n  %s", i.title, i.desc)
	if index == m.Index() {
		fmt.Fprint(w, SelectedItemStyle.Render(str))
	} else {
		fmt.Fprint(w, NormalItemStyle.Render(str))
	}
}

type nearScreen struct {
	env    *env
	modes  list.Model
	mode   string // "" while choosing
	input  textinput.Model
	deep   bool
	focus  int
	run    running
	asked  string
	status status
	out    Outcome
}

func newNearScreen(e *env) *nearScreen {
	modes := newList([]list.Item{
		nearItem{"📂 A stored object", "Find what shares content with an object in the database", nearIn},
		nearItem{"📁 A file outside the database", "Find what shares content with a file on disk", nearOut},
	}, nearDelegate{}, "NeAR")
	in := textinput.New()
	return &nearScreen{env: e, modes: modes, input: in}
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
	if s.mode == nearOut && f == nearFocusDeep {
		// The TUI offers deep mode for stored objects only; skip it.
		if s.focus == nearFocusAnalyze {
			f = nearFocusInput
		} else {
			f = nearFocusAnalyze
		}
	}
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
		case nearFocusDeep:
			s.deep = !s.deep
		}
		return nil
	case "space":
		if s.focus == nearFocusDeep {
			s.deep = !s.deep
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
	var fn func(context.Context) (any, error)
	switch s.mode {
	case nearIn:
		nearInFn, deep := s.env.actions.NearIn, s.deep
		if nearInFn == nil {
			s.status = status{statusError, "Error: NeAR is not available"}
			return nil
		}
		name = "near " + truncate(input, 12)
		fn = func(ctx context.Context) (any, error) { return nearInFn(ctx, input, deep) }
	case nearOut:
		nearOutFn := s.env.actions.NearOut
		if nearOutFn == nil {
			s.status = status{statusError, "Error: NeAR is not available"}
			return nil
		}
		name = "near " + filepath.Base(input)
		fn = func(ctx context.Context) (any, error) { return nearOutFn(ctx, input) }
	default:
		return nil
	}
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
	e := s.env
	title := TitleStyle.Render("NeAR: find stored objects that share content")
	if s.mode == "" {
		return frame(e.width, e.height, joinLines(
			title,
			s.modes.View(),
			HelpStyle.Render("↑/↓: Choose | Enter: Select | esc: Back"),
		))
	}
	label := "Stored object (ID or hash):"
	if s.mode == nearOut {
		label = "File:"
	}
	if s.focus == nearFocusInput {
		label = SelectedItemStyle.Render(label)
	}
	parts := []string{title, InputStyle.Render(label + "\n" + s.input.View())}
	if s.mode == nearIn {
		deep := checkbox(s.deep, "Deep: also match similar chunks (slower)")
		if s.focus == nearFocusDeep {
			deep = SelectedItemStyle.Render(deep)
		}
		parts = append(parts, deep)
	}
	stat := s.status.render()
	if s.run.active() {
		stat = InfoStyle.Render(fmt.Sprintf("Analysing %s… %s", clean(s.asked), since(s.run.since)))
	}
	parts = append(parts,
		lipgloss.JoinHorizontal(lipgloss.Left, button("Analyse", s.focus == nearFocusAnalyze)),
		DimStyle.Render("The report replaces near_report.json in the working directory."),
		stat,
		outcomeLines(s.out, e.width-8),
		HelpStyle.Render("Tab/↑/↓: Move | Enter: Select | Space: Toggle | esc: Choose again | ctrl+x: Cancel the analysis"),
	)
	return frame(e.width, e.height, joinLines(parts...))
}
