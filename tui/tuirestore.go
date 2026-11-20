package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const (
	restoreFocusID = iota
	restoreFocusPath
	restoreFocusRestore
	restoreFocusCancel
	restoreFields
)

// defaultRestoreName is where the restore screen writes when no path is given.
const defaultRestoreName = "restored"

type restoreScreen struct {
	env     *env
	id      textinput.Model
	path    textinput.Model
	focus   int
	run     running
	target  string // the path being restored to
	status  status
	out     Outcome
	replace prompt // the target exists: replace it?
	another prompt // after a restore: restore another?
}

func newRestoreScreen(e *env) *restoreScreen {
	id := e.newInput("ID or hash of a stored object…")
	id.Focus()
	path := e.newInput("Path to restore to (default: " + defaultRestoreName + ")")
	return &restoreScreen{env: e, id: id, path: path}
}

func (s *restoreScreen) restyle() {
	s.id.SetStyles(s.env.st.textinput)
	s.path.SetStyles(s.env.st.textinput)
}

func (s *restoreScreen) init() tea.Cmd { return textinput.Blink }
func (s *restoreScreen) job() int      { return s.run.id }

func (s *restoreScreen) resize() {
	w := max(24, s.env.width-14)
	s.id.SetWidth(w)
	s.path.SetWidth(w)
}

func (s *restoreScreen) typing() bool {
	return (s.focus == restoreFocusID || s.focus == restoreFocusPath) && !s.replace.open && !s.another.open
}

// escape answers No to the replace prompt.
func (s *restoreScreen) escape() bool {
	if s.replace.open {
		s.replace.open = false
		s.status = infoStatus("Not restored: " + clean(s.target) + " exists")
		return true
	}
	return false
}

func (s *restoreScreen) setFocus(f int) {
	s.focus = f
	s.id.Blur()
	s.path.Blur()
	switch f {
	case restoreFocusID:
		s.id.Focus()
	case restoreFocusPath:
		s.path.Focus()
	}
}

func (s *restoreScreen) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case jobDoneMsg:
		s.run = running{}
		s.out, _ = msg.result.(Outcome)
		switch {
		case errors.Is(msg.err, ErrTargetExists):
			s.status = status{}
			s.replace.ask(clean(s.target)+" exists. Replace it?", "Yes, replace it", "No", false)
			return nil
		case msg.err != nil:
			s.status = errStatus(msg.err)
			return nil
		case s.out.Warning:
			s.status = status{statusWarn, "Restored, with warnings: see below"}
		default:
			s.status = status{statusOK, "Restored"}
		}
		s.setFocus(-1)
		s.another.ask("Restore another object?", "Yes", "No", true)
		return nil
	case tea.KeyPressMsg:
		switch {
		case s.replace.open:
			if decided, yes := s.replace.key(msg); decided {
				if yes {
					return s.start(true)
				}
				s.status = infoStatus("Not restored: " + clean(s.target) + " exists")
			}
			return nil
		case s.another.open:
			decided, yes := s.another.key(msg)
			switch {
			case !decided:
				return nil
			case yes:
				s.id.SetValue("")
				s.path.SetValue("")
				s.out = Outcome{}
				s.status = infoStatus("Ready to restore another object")
				s.setFocus(restoreFocusID)
				return nil
			}
			return back
		}
		return s.key(msg)
	}
	return s.updateInput(msg)
}

func (s *restoreScreen) updateInput(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	switch s.focus {
	case restoreFocusID:
		s.id, cmd = s.id.Update(msg)
	case restoreFocusPath:
		s.path, cmd = s.path.Update(msg)
	}
	return cmd
}

func (s *restoreScreen) key(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "tab", "down":
		s.setFocus((s.focus + 1) % restoreFields)
		return nil
	case "shift+tab", "up":
		s.setFocus((s.focus + restoreFields - 1) % restoreFields)
		return nil
	case "enter":
		switch s.focus {
		case restoreFocusID:
			s.setFocus(restoreFocusPath)
		case restoreFocusPath, restoreFocusRestore:
			return s.start(false)
		case restoreFocusCancel:
			return back
		}
		return nil
	}
	return s.updateInput(msg)
}

// start restores; replace is set once the user agreed to replace the target.
func (s *restoreScreen) start(replace bool) tea.Cmd {
	if s.run.active() {
		return nil
	}
	id := strings.TrimSpace(s.id.Value())
	if id == "" {
		s.status = status{statusError, "Error: enter the ID or hash of a stored object"}
		return nil
	}
	target := strings.TrimSpace(s.path.Value())
	if target == "" {
		target = defaultRestoreName
	}
	restore := s.env.actions.Restore
	if restore == nil {
		s.status = status{statusError, "Error: restoring is not available"}
		return nil
	}
	jobID, cmd, err := s.env.run("restore "+truncate(id, 12), sharedJob, func(ctx context.Context) (any, error) {
		return restore(ctx, id, target, replace)
	})
	if err != nil {
		s.status = errStatus(err)
		return nil
	}
	s.run = running{id: jobID, since: time.Now()}
	s.target = target
	s.out = Outcome{}
	s.status = status{}
	return cmd
}

func (s *restoreScreen) view() string {
	e, st := s.env, s.env.st
	idLabel, pathLabel := "ID or hash:", "Restore to:"
	if s.focus == restoreFocusID {
		idLabel = st.selected.Render(idLabel)
	}
	if s.focus == restoreFocusPath {
		pathLabel = st.selected.Render(pathLabel)
	}
	buttons := lipgloss.JoinHorizontal(lipgloss.Left,
		button(st, "Restore", s.focus == restoreFocusRestore), button(st, "Cancel", s.focus == restoreFocusCancel))
	stat := s.status.render(st)
	if s.run.active() {
		stat = st.info.Render(fmt.Sprintf("Restoring to %s… %s", clean(s.target), since(s.run.since)))
	}
	help := "Tab/↑/↓: Move | Enter: Select | esc: Back (a restore keeps running) | ctrl+x: Cancel the restore"
	if s.replace.open || s.another.open {
		help = ""
	}
	return e.frame(joinLines(
		st.title.Render("Restore a stored object"),
		st.input.Render(idLabel+"\n"+s.id.View())+"\n"+st.input.Render(pathLabel+"\n"+s.path.View()),
		st.dim.Render("An existing file is replaced only after you confirm. Every chunk, and the whole content, is checked against its hash."),
		buttons,
		stat,
		s.replace.view(st),
		outcomeLines(s.out, e.width-8),
		s.another.view(st),
		st.help.Render(help),
	))
}
