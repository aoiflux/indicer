package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// running is the job a screen waits for.
type running struct {
	id    int
	since time.Time
}

func (r running) active() bool { return r.id != 0 }

// prompt is a yes/no question on a screen. No is chosen unless the screen
// says otherwise.
type prompt struct {
	open     bool
	question string
	yes, no  string
	choice   bool // true: yes
}

func (p *prompt) ask(question, yes, no string, choice bool) {
	*p = prompt{open: true, question: question, yes: yes, no: no, choice: choice}
}

// key handles a key while the prompt is open. decided is set when the
// prompt closed, and accepted when the answer was yes.
func (p *prompt) key(msg tea.KeyPressMsg) (decided, accepted bool) {
	switch msg.String() {
	case "tab", "shift+tab", "left", "right":
		p.choice = !p.choice
	case "y":
		p.open = false
		return true, true
	case "n":
		p.open = false
		return true, false
	case "enter":
		p.open = false
		return true, p.choice
	}
	return false, false
}

func (p prompt) view() string {
	if !p.open {
		return ""
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		WarningStyle.Render(p.question),
		lipgloss.JoinHorizontal(lipgloss.Left, button(p.no, !p.choice), button(p.yes, p.choice)),
		HelpStyle.Render("Tab/←/→: Choose | Enter: Confirm | y/n | esc: No"),
	)
}
