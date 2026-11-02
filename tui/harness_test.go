package tui

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"indicer/internal/ui"

	tea "charm.land/bubbletea/v2"
)

// The harness drives the model as the program does: a key is an Update, the
// command it returns runs on a goroutine of its own, and the message the
// command returns is the next Update. Blinks and ticks are dropped; a quit is
// recorded.

// key is the key press s names: a key name, or one printable character.
func key(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEsc}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "ctrl+x":
		return tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl}
	}
	r := []rune(s)
	if len(r) != 1 {
		panic("key: unknown key " + s)
	}
	return tea.KeyPressMsg{Code: r[0], Text: s}
}

type driver struct {
	t       *testing.T
	m       *model
	jobs    *runner
	results chan tea.Msg
	quit    bool
}

// testInfo's root is relative, so that its folder name is db on every
// system.
var testInfo = Info{Root: filepath.Join("cases", "db"), ChunkKiB: 256, ChunkHash: "blake3"}

// newDriver shows the menu of a model on acts, under --theme auto with no
// answer from the terminal yet. Every job is stopped when the test ends,
// and the test fails if one does not stop.
func newDriver(t *testing.T, acts Actions) *driver {
	t.Helper()
	return newThemedDriver(t, acts, ui.ThemeUnknown)
}

// newThemedDriver is newDriver under --theme theme.
func newThemedDriver(t *testing.T, acts Actions, theme ui.Theme) *driver {
	t.Helper()
	jobs := newRunner(t.Context())
	d := &driver{t: t, m: newModel(Config{Info: testInfo, Actions: acts, Theme: theme}, jobs), jobs: jobs, results: make(chan tea.Msg, 1024)}
	t.Cleanup(func() {
		jobs.close()
		jobs.cancelAll(errStopping)
		if !jobs.wait(5 * time.Second) {
			t.Errorf("jobs still running at the end of the test: %v", jobs.running())
		}
	})
	return d
}

// send delivers msg and runs the command it returns.
func (d *driver) send(msg tea.Msg) {
	d.t.Helper()
	_, cmd := d.m.Update(msg)
	d.exec(cmd)
}

// press presses each key in turn.
func (d *driver) press(keys ...string) {
	d.t.Helper()
	for _, k := range keys {
		d.send(key(k))
	}
}

// typeText types s, one key per character.
func (d *driver) typeText(s string) {
	d.t.Helper()
	for _, r := range s {
		d.send(key(string(r)))
	}
}

func (d *driver) exec(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() { d.results <- cmd() }()
}

// handle is what the program does with a command's message.
func (d *driver) handle(msg tea.Msg) {
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			d.exec(c)
		}
	case tea.QuitMsg:
		d.quit = true
	case tickMsg:
	default:
		if t := reflect.TypeOf(msg); t.PkgPath() == "charm.land/bubbles/v2/cursor" {
			return // a cursor blink
		}
		d.send(msg)
	}
}

// until delivers messages until cond holds, and fails the test if it does
// not within a few seconds.
func (d *driver) until(what string, cond func() bool) {
	d.t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case msg := <-d.results:
			d.handle(msg)
		case <-deadline:
			d.t.Fatalf("timed out waiting until %s; screen:\n%s", what, d.view())
		}
	}
}

// settle delivers the messages that arrive within a short while: for checks
// that something did not happen.
func (d *driver) settle() {
	d.t.Helper()
	for {
		select {
		case msg := <-d.results:
			d.handle(msg)
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}

func (d *driver) view() string { return d.m.View().Content }

// shows reports whether the screen shows s.
func (d *driver) shows(s string) bool { return strings.Contains(d.view(), s) }

// open opens the menu entry st from the menu.
func (d *driver) open(st State) {
	d.t.Helper()
	if d.m.state != StateMenu {
		d.t.Fatalf("open %d: not on the menu (state %d)", st, d.m.state)
	}
	d.m.menu.Select(int(st) - 1)
	d.press("enter")
	if d.m.state != st {
		d.t.Fatalf("open %d: state is %d", st, d.m.state)
	}
}

// record is a list a fake action adds to from its job's goroutine, while
// the test reads it.
type record[T any] struct {
	mu   sync.Mutex
	list []T
}

func (r *record[T]) add(v T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, v)
}

func (r *record[T]) get() []T {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.list)
}

func (r *record[T]) len() int { return len(r.get()) }

// gate is a job that runs until the test releases it or its context ends.
type gate struct {
	started chan struct{}
	release chan struct{}
	// ignoreCancel makes the job ignore its context.
	ignoreCancel bool

	mu    sync.Mutex
	calls int
	cause error // the context's cause, once it ended
}

func newGate() *gate {
	return &gate{started: make(chan struct{}, 16), release: make(chan struct{})}
}

func (g *gate) wait(ctx context.Context) error {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	g.started <- struct{}{}
	if g.ignoreCancel {
		<-g.release
		return nil
	}
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		g.mu.Lock()
		g.cause = context.Cause(ctx)
		g.mu.Unlock()
		return ctx.Err()
	}
}

// waitStarted waits for the job to start.
func (g *gate) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-g.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the job did not start")
	}
}

func (g *gate) ended() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cause
}

func (g *gate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// storeWith is a store action that waits on g and then reports out.
func storeWith(g *gate, out Outcome) func(context.Context, string, StoreOptions) (Outcome, error) {
	return func(ctx context.Context, _ string, _ StoreOptions) (Outcome, error) {
		if err := g.wait(ctx); err != nil {
			return Outcome{}, err
		}
		return out, nil
	}
}
