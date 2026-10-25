package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"indicer/internal/lifecycle"
	"indicer/internal/safe"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// drainTimeout is how long Run waits for jobs to stop once the program has
// ended. Tests shorten it.
var drainTimeout = 30 * time.Second

// Config is what Run needs.
type Config struct {
	Info    Info
	Actions Actions
	// Options are added to the program's own; tests set its input and
	// output.
	Options []tea.ProgramOption
}

// Run shows the TUI until the user quits or ctx ends, then waits for its
// jobs. It returns ErrJobsRunning when one has still not stopped after
// drainTimeout: the caller must then leave the database open for badger to
// recover at its next open, not close it under the job (TUI-3).
func Run(ctx context.Context, cfg Config) error {
	jobs := newRunner(ctx)
	m := newModel(cfg.Info, cfg.Actions, jobs)
	// A signal ends the program through ctx, with its cause (exit 130/143).
	// bubbletea's own handler would end it without one.
	opts := append([]tea.ProgramOption{tea.WithContext(ctx), tea.WithoutSignalHandler()}, cfg.Options...)
	_, runErr := tea.NewProgram(m, opts...).Run()

	// bubbletea does not wait for the commands it started; the jobs are
	// waited for here, before the database can be closed.
	jobs.close()
	jobs.cancelAll(errStopping)
	if !jobs.wait(drainTimeout) {
		var names []string
		for _, j := range jobs.running() {
			names = append(names, clean(j.name))
		}
		return fmt.Errorf("%w (%s); the database was left for recovery at its next open", ErrJobsRunning, strings.Join(names, ", "))
	}
	switch {
	case m.fatal != nil:
		return &lifecycle.InternalError{Err: m.fatal}
	case ctx.Err() != nil:
		return context.Cause(ctx)
	case errors.Is(runErr, tea.ErrProgramPanic):
		return &lifecycle.InternalError{Err: runErr}
	}
	return runErr
}

// State is the screen showing.
type State int

const (
	StateMenu State = iota
	StateStore
	StateList
	StateSearch
	StateRestore
	StateNear
	StateReset
	StateStats
	StateMicro
)

// env is what every screen shares.
type env struct {
	info    Info
	actions Actions
	jobs    *runner
	width   int
	height  int
}

// run starts fn as a job, and returns its ID and the command that reports
// its end.
func (e *env) run(name string, kind jobKind, fn func(context.Context) (any, error)) (int, tea.Cmd, error) {
	j, err := e.jobs.start(name, kind, fn)
	if err != nil {
		return 0, nil, err
	}
	return j.id, await(j), nil
}

// screen is one of the menu's screens.
type screen interface {
	init() tea.Cmd
	update(msg tea.Msg) tea.Cmd
	view() string
	resize()
	// typing reports whether a text input has focus, so that q is text.
	typing() bool
	// job is the ID of the job the screen waits for, or 0.
	job() int
}

// escaper is a screen with a prompt of its own, which esc closes before it
// leaves the screen.
type escaper interface {
	escape() bool
}

// backMsg returns to the menu.
type backMsg struct{}

func back() tea.Msg { return backMsg{} }

// tickMsg redraws the elapsed times of running jobs.
type tickMsg struct{}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

type quitState int

const (
	quitNone quitState = iota
	quitAsking
	quitStopping
)

type model struct {
	env    *env
	state  State
	menu   list.Model
	screen screen // nil on the menu
	// status is the last finished job, shown on the menu.
	status status
	quit   quitState
	// quitYes is the quit dialog's choice: No unless changed.
	quitYes bool
	// fatal is a panic contained in the model; the program ends with it.
	fatal error
}

func newModel(info Info, acts Actions, jobs *runner) *model {
	e := &env{info: info, actions: acts, jobs: jobs, width: 100, height: 30}
	m := &model{env: e, state: StateMenu, menu: newMenu()}
	m.resize()
	return m
}

func (m *model) Init() tea.Cmd { return tick() }

// Update contains a panic in the model: it is logged, every job is
// cancelled, and the program ends; Run waits for the jobs.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	err := safe.Call(safe.Site{Component: "tui", Op: "update"}, func() error {
		cmd = m.update(msg)
		return nil
	})
	if err != nil {
		m.fatal = err
		m.env.jobs.cancelAll(errStopping)
		return m, tea.Quit
	}
	return m, cmd
}

func (m *model) View() tea.View {
	content, err := safe.Value(safe.Site{Component: "tui", Op: "view"}, func() (string, error) {
		return m.view(), nil
	})
	if err != nil {
		// View cannot end the program: the next key does.
		m.fatal = err
		m.env.jobs.cancelAll(errStopping)
		content = ErrorStyle.Render("Internal error: "+errorText(err)) + "\n\nPress any key to exit."
	}
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

func (m *model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.env.width, m.env.height = msg.Width, msg.Height
		m.resize()
		return nil
	case tickMsg:
		return tick()
	case jobDoneMsg:
		return m.jobDone(msg)
	case backMsg:
		m.back()
		return nil
	case tea.KeyPressMsg:
		return m.key(msg)
	}
	if m.screen != nil {
		return m.screen.update(msg)
	}
	var cmd tea.Cmd
	m.menu, cmd = m.menu.Update(msg)
	return cmd
}

// jobDone reports a finished job to the screen that started it, if it still
// shows, and on the menu's status line in any case: leaving a screen never
// loses its job's outcome.
func (m *model) jobDone(msg jobDoneMsg) tea.Cmd {
	var cmd tea.Cmd
	if m.screen != nil && m.screen.job() == msg.id {
		cmd = m.screen.update(msg)
	}
	if s, ok := summarize(msg); ok {
		m.status = s
	}
	if m.quit == quitStopping && !m.env.jobs.busy() {
		return tea.Quit
	}
	return cmd
}

// summarize is a finished job's line on the menu. A loader reports only a
// failure.
func summarize(msg jobDoneMsg) (status, bool) {
	name := clean(msg.name)
	if msg.err != nil {
		return status{statusError, name + ": " + errorText(msg.err)}, true
	}
	if msg.kind == loaderJob {
		return status{}, false
	}
	o, ok := msg.result.(Outcome)
	if !ok {
		return status{}, false
	}
	text := name + ": done"
	if len(o.Lines) > 0 {
		text = name + ": " + clean(o.Lines[0])
	}
	if o.Warning {
		return status{statusWarn, text + " (with warnings)"}, true
	}
	return status{statusOK, text}, true
}

func (m *model) key(msg tea.KeyPressMsg) tea.Cmd {
	if m.fatal != nil {
		return tea.Quit
	}
	if m.quit != quitNone {
		return m.quitKey(msg)
	}
	switch msg.String() {
	case "ctrl+c":
		return m.requestQuit()
	case "ctrl+x":
		m.cancelJobs()
		return nil
	}
	if m.screen == nil {
		return m.menuKey(msg)
	}
	switch msg.String() {
	case "esc":
		if e, ok := m.screen.(escaper); ok && e.escape() {
			return nil
		}
		m.back()
		return nil
	case "q":
		// q is text while an input has focus (TUI-4).
		if !m.screen.typing() {
			m.back()
			return nil
		}
	}
	return m.screen.update(msg)
}

func (m *model) menuKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "q", "esc":
		return m.requestQuit()
	case "enter":
		if item, ok := m.menu.SelectedItem().(menuItem); ok {
			return m.open(item.state)
		}
		return nil
	}
	var cmd tea.Cmd
	m.menu, cmd = m.menu.Update(msg)
	return cmd
}

// open shows a screen, fresh.
func (m *model) open(s State) tea.Cmd {
	var sc screen
	switch s {
	case StateStore:
		sc = newStoreScreen(m.env)
	case StateList:
		sc = newListScreen(m.env)
	case StateSearch:
		sc = newSearchScreen(m.env)
	case StateRestore:
		sc = newRestoreScreen(m.env)
	case StateNear:
		sc = newNearScreen(m.env)
	case StateReset:
		sc = newResetScreen(m.env)
	case StateStats:
		sc = newStatsScreen(m.env)
	case StateMicro:
		sc = newMicroScreen(m.env)
	default:
		return nil
	}
	m.state, m.screen = s, sc
	sc.resize()
	return sc.init()
}

// back returns to the menu. A job the screen started keeps running; its
// outcome shows on the menu.
func (m *model) back() {
	m.state, m.screen = StateMenu, nil
}

// cancelJobs cancels the screen's job, or, on the menu, every job.
func (m *model) cancelJobs() {
	if m.screen != nil {
		if id := m.screen.job(); id != 0 {
			m.env.jobs.cancel(id, errCanceled)
		}
		return
	}
	if m.env.jobs.busy() {
		m.env.jobs.cancelAll(errCanceled)
		m.status = infoStatus("Cancelling every running job…")
	}
}

// requestQuit quits, or, while jobs run, asks first (TUI-3).
func (m *model) requestQuit() tea.Cmd {
	if !m.env.jobs.busy() {
		return tea.Quit
	}
	m.quit, m.quitYes = quitAsking, false
	return nil
}

func (m *model) quitKey(msg tea.KeyPressMsg) tea.Cmd {
	if m.quit == quitStopping {
		if msg.String() == "ctrl+c" {
			// Quit now; Run still waits for the jobs before the
			// database is closed.
			return tea.Quit
		}
		return nil
	}
	switch msg.String() {
	case "tab", "shift+tab", "left", "right":
		m.quitYes = !m.quitYes
	case "y", "ctrl+c":
		return m.stop()
	case "n", "esc", "q":
		m.quit = quitNone
	case "enter":
		if m.quitYes {
			return m.stop()
		}
		m.quit = quitNone
	}
	return nil
}

// stop cancels every job and quits once they have stopped.
func (m *model) stop() tea.Cmd {
	m.quit = quitStopping
	m.env.jobs.cancelAll(errStopping)
	if !m.env.jobs.busy() {
		return tea.Quit
	}
	return nil
}

func (m *model) resize() {
	m.menu.SetWidth(max(40, m.env.width-8))
	m.menu.SetHeight(max(8, m.env.height-14))
	if m.screen != nil {
		m.screen.resize()
	}
}

func (m *model) view() string {
	if m.quit != quitNone {
		return m.quitView()
	}
	var body string
	if m.screen == nil {
		body = m.menuView()
	} else {
		body = m.screen.view()
	}
	if f := m.footer(); f != "" {
		body = lipgloss.JoinVertical(lipgloss.Left, body, f)
	}
	return body
}

// footer lists the jobs running behind the screen.
func (m *model) footer() string {
	own := 0
	if m.screen != nil {
		own = m.screen.job()
	}
	var parts []string
	for _, j := range m.env.jobs.running() {
		if j.id != own {
			parts = append(parts, fmt.Sprintf("%s (%s)", clean(j.name), since(j.since)))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	hint := "ctrl+x on the menu cancels"
	if m.screen == nil {
		hint = "ctrl+x cancels"
	}
	return WarningStyle.Render("Running: "+strings.Join(parts, ", ")) + "  " + HelpStyle.Render(hint)
}

func (m *model) menuView() string {
	info := m.env.info
	lines := []string{
		HeaderStyle.Render("DUES - Deduplicated Unified Evidence Store"),
		InfoStyle.Render("Database: " + clean(info.Root)),
		DimStyle.Render(fmt.Sprintf("Chunks of %d KiB, hashed with %s", info.ChunkKiB, clean(info.ChunkHash))),
	}
	if !info.Encrypted {
		lines = append(lines, WarningStyle.Render("This database is not encrypted."))
	}
	if m.status.text != "" {
		lines = append(lines, "Last: "+m.status.render())
	}
	lines = append(lines, "", m.menu.View(), "", HelpStyle.Render("↑/↓: Navigate | Enter: Select | q: Quit | ctrl+x: Cancel running jobs"))
	return frame(m.env.width, m.env.height, lipgloss.JoinVertical(lipgloss.Left, lines...))
}

func (m *model) quitView() string {
	var names []string
	for _, j := range m.env.jobs.running() {
		names = append(names, fmt.Sprintf("%s (%s)", clean(j.name), since(j.since)))
	}
	var content string
	if m.quit == quitAsking {
		content = lipgloss.JoinVertical(lipgloss.Left,
			TitleStyle.Render("Quit DUES?"),
			"",
			WarningStyle.Render(fmt.Sprintf("%d job(s) running: %s", len(names), strings.Join(names, ", "))),
			"",
			"Stop them and quit?",
			"",
			lipgloss.JoinHorizontal(lipgloss.Left, button("No, keep working", !m.quitYes), button("Yes, stop and quit", m.quitYes)),
			"",
			HelpStyle.Render("Tab/←/→: Choose | Enter: Confirm | y/n"),
		)
	} else {
		content = lipgloss.JoinVertical(lipgloss.Left,
			TitleStyle.Render("Stopping"),
			"",
			"Waiting for: "+strings.Join(names, ", "),
			"",
			HelpStyle.Render(fmt.Sprintf("ctrl+c: Quit now. DUES still waits up to %v for the jobs, and leaves the database for recovery if one has not stopped.", drainTimeout)),
		)
	}
	box := DialogBoxStyle
	if m.env.width > 0 {
		box = box.Width(max(40, min(m.env.width-4, 90)))
	}
	return box.Render(content)
}

// menuItem is one entry of the menu.
type menuItem struct {
	title, description string
	state              State
}

func (i menuItem) FilterValue() string { return i.title }

type menuDelegate struct{}

func (menuDelegate) Height() int                         { return 2 }
func (menuDelegate) Spacing() int                        { return 0 }
func (menuDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }
func (menuDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	i, ok := item.(menuItem)
	if !ok {
		return
	}
	str := fmt.Sprintf("  %s\n  %s", i.title, i.description)
	if index == m.Index() {
		fmt.Fprint(w, SelectedItemStyle.Render(str))
	} else {
		fmt.Fprint(w, NormalItemStyle.Render(str))
	}
}

func newMenu() list.Model {
	items := []list.Item{
		menuItem{"💾 Store", "Store a file or folder of evidence", StateStore},
		menuItem{"📂 List", "List the stored evidence", StateList},
		menuItem{"🔍 Search", "Search the stored content", StateSearch},
		menuItem{"🔄 Restore", "Restore a stored object to a file", StateRestore},
		menuItem{"🔎 NeAR", "Find stored objects that share content", StateNear},
		menuItem{"🗑️  Reset", "Delete everything in the database", StateReset},
		menuItem{"📊 Statistics", "Show the database's statistics", StateStats},
		menuItem{"🔬 Micro-artefacts", "Show the files with the most micro-artefacts", StateMicro},
	}
	return newList(items, menuDelegate{}, "DUES")
}

// newList is a list without filtering, quit keys or help of its own: the
// screens handle those keys (TUI-4).
func newList(items []list.Item, d list.ItemDelegate, title string) list.Model {
	l := list.New(items, d, 60, 16)
	l.Title = title
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)
	l.SetShowHelp(false)
	l.DisableQuitKeybindings()
	return l
}
