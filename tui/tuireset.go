package tui

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const (
	resetFocusInput = iota
	resetFocusNo
	resetFocusYes
	resetFields
)

// resetScreen empties the database. It asks for the data root's folder name,
// as `dues reset` does; No is the default, and enter in the name field never
// resets (TUI-1).
type resetScreen struct {
	env    *env
	name   string
	input  textinput.Model
	focus  int
	run    running
	status status
	done   bool
}

func newResetScreen(e *env) *resetScreen {
	in := textinput.New()
	in.Placeholder = "Folder name"
	in.Focus()
	return &resetScreen{env: e, name: filepath.Base(e.info.Root), input: in}
}

func (s *resetScreen) init() tea.Cmd { return textinput.Blink }
func (s *resetScreen) resize()       { s.input.SetWidth(max(24, min(60, s.env.width-14))) }
func (s *resetScreen) job() int      { return s.run.id }
func (s *resetScreen) typing() bool  { return s.focus == resetFocusInput && !s.done }

func (s *resetScreen) confirmed() bool {
	return s.name != "" && strings.TrimSpace(s.input.Value()) == s.name
}

// blocked says why the reset cannot run now, or "".
func (s *resetScreen) blocked() string {
	if jobs := s.env.jobs.running(); len(jobs) > 0 {
		names := make([]string, len(jobs))
		for i, j := range jobs {
			names[i] = clean(j.name)
		}
		return "A reset cannot run while other jobs run (" + strings.Join(names, ", ") + "). Wait for them, or cancel them with ctrl+x on the menu."
	}
	return ""
}

func (s *resetScreen) setFocus(f int) {
	s.focus = f
	if f == resetFocusInput {
		s.input.Focus()
	} else {
		s.input.Blur()
	}
}

func (s *resetScreen) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case jobDoneMsg:
		s.run = running{}
		s.done = true
		if msg.err != nil {
			s.status = errStatus(msg.err)
		} else {
			s.status = status{statusOK, "The database is empty."}
		}
		return nil
	case tea.KeyPressMsg:
		if s.done {
			if msg.String() == "enter" {
				return back
			}
			return nil
		}
		if s.run.active() {
			return nil
		}
		return s.key(msg)
	}
	if s.focus == resetFocusInput {
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return cmd
	}
	return nil
}

func (s *resetScreen) key(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "tab", "down", "right":
		if msg.String() == "right" && s.focus == resetFocusInput {
			break // moves the cursor in the field
		}
		s.setFocus((s.focus + 1) % resetFields)
		return nil
	case "shift+tab", "up", "left":
		if msg.String() == "left" && s.focus == resetFocusInput {
			break
		}
		s.setFocus((s.focus + resetFields - 1) % resetFields)
		return nil
	case "enter":
		switch s.focus {
		case resetFocusInput:
			// Never resets: the No button comes first.
			s.setFocus(resetFocusNo)
		case resetFocusNo:
			return back
		case resetFocusYes:
			return s.start()
		}
		return nil
	}
	if s.focus == resetFocusInput {
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return cmd
	}
	return nil
}

func (s *resetScreen) start() tea.Cmd {
	if !s.confirmed() {
		s.status = status{statusError, "Type " + clean(s.name) + " to confirm."}
		return nil
	}
	if why := s.blocked(); why != "" {
		s.status = status{statusError, why}
		return nil
	}
	reset := s.env.actions.Reset
	if reset == nil {
		s.status = status{statusError, "Error: reset is not available"}
		return nil
	}
	id, cmd, err := s.env.run("reset", exclusiveJob, func(ctx context.Context) (any, error) {
		return Outcome{Lines: []string{"the database is empty"}}, reset(ctx)
	})
	if err != nil {
		s.status = errStatus(err)
		return nil
	}
	s.run = running{id: id, since: time.Now()}
	s.status = infoStatus("Resetting…")
	return cmd
}

func (s *resetScreen) view() string {
	e := s.env
	title := TitleStyle.Render("⚠️  Reset the database")
	warning := ErrorStyle.Render("This deletes everything in " + clean(e.info.Root) + ":\nall stored evidence, indexes, the graph and the full-text index. It cannot be undone.")
	if s.done {
		return frame(e.width, e.height, joinLines(title, s.status.render(), HelpStyle.Render("Enter/esc: Back to the menu")))
	}
	label := "Type " + clean(s.name) + " to confirm:"
	if s.focus == resetFocusInput {
		label = SelectedItemStyle.Render(label)
	}
	no := button("No, keep everything", s.focus == resetFocusNo)
	yes := disabledButton("Yes, delete everything")
	if s.confirmed() && s.blocked() == "" {
		yes = button("Yes, delete everything", s.focus == resetFocusYes)
	} else if s.focus == resetFocusYes {
		yes = SelectedItemStyle.Render("[") + yes + SelectedItemStyle.Render("]")
	}
	blocked := ""
	if why := s.blocked(); why != "" && !s.run.active() {
		blocked = WarningStyle.Render(why)
	}
	return frame(e.width, e.height, joinLines(
		title,
		warning,
		InputStyle.Render(label+"\n"+s.input.View()),
		lipgloss.JoinHorizontal(lipgloss.Left, no, yes),
		blocked,
		s.status.render(),
		HelpStyle.Render("Tab/↑/↓: Move | Enter: Select | esc: Back"),
	))
}
