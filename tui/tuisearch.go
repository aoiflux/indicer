package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

type searchScreen struct {
	env    *env
	query  textinput.Model
	run    running
	asked  string // the query searched for
	status status
	out    Outcome
}

func newSearchScreen(e *env) *searchScreen {
	in := textinput.New()
	in.Placeholder = `Words (ANDed), a | b, or "a phrase"…`
	in.Focus()
	return &searchScreen{env: e, query: in}
}

func (s *searchScreen) init() tea.Cmd { return textinput.Blink }
func (s *searchScreen) resize()       { s.query.SetWidth(max(24, s.env.width-14)) }
func (s *searchScreen) typing() bool  { return true }
func (s *searchScreen) job() int      { return s.run.id }

func (s *searchScreen) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case jobDoneMsg:
		s.run = running{}
		s.out, _ = msg.result.(Outcome)
		switch {
		case msg.err != nil:
			s.status = errStatus(msg.err)
		case s.out.Warning:
			s.status = status{statusWarn, "Searched, with warnings: see below"}
		default:
			s.status = status{statusOK, "Searched"}
		}
		return nil
	case tea.KeyPressMsg:
		if msg.String() == "enter" {
			return s.start()
		}
	}
	var cmd tea.Cmd
	s.query, cmd = s.query.Update(msg)
	return cmd
}

func (s *searchScreen) start() tea.Cmd {
	if s.run.active() {
		return nil
	}
	query := strings.TrimSpace(s.query.Value())
	if query == "" {
		s.status = status{statusError, "Error: enter something to search for"}
		return nil
	}
	search := s.env.actions.Search
	if search == nil {
		s.status = status{statusError, "Error: searching is not available"}
		return nil
	}
	id, cmd, err := s.env.run("search "+truncate(query, 24), sharedJob, func(ctx context.Context) (any, error) {
		return search(ctx, query)
	})
	if err != nil {
		s.status = errStatus(err)
		return nil
	}
	s.run = running{id: id, since: time.Now()}
	s.asked = query
	s.out = Outcome{}
	s.status = status{}
	return cmd
}

func (s *searchScreen) view() string {
	e := s.env
	stat := s.status.render()
	if s.run.active() {
		stat = InfoStyle.Render(fmt.Sprintf("Searching for %s… %s", clean(s.asked), since(s.run.since)))
	}
	return frame(e.width, e.height, joinLines(
		TitleStyle.Render("Search the stored content"),
		InputStyle.Render(SelectedItemStyle.Render("Search for:")+"\n"+s.query.View()),
		DimStyle.Render("Every stored object is searched. The report replaces report.json in the working directory."),
		stat,
		outcomeLines(s.out, e.width-8),
		HelpStyle.Render("Enter: Search | esc: Back (a search keeps running) | ctrl+x: Cancel the search"),
	))
}
