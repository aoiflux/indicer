package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"indicer/lib/microartefact"
	"indicer/lib/store"

	tea "charm.land/bubbletea/v2"
)

// TestQuitWhileJobsRunAsks (TUI-3): ctrl+c while a store runs asks first,
// with No chosen; yes stops the store and quits once it has stopped.
func TestQuitWhileJobsRunAsks(t *testing.T) {
	g := newGate()
	d := newDriver(t, Actions{Store: storeWith(g, Outcome{})})
	d.open(StateStore)
	d.typeText("case.e01")
	d.press("enter")
	g.waitStarted(t)

	d.press("ctrl+c")
	d.settle()
	if d.quit || d.m.quit != quitAsking || !d.shows("Quit DUES?") || !d.shows("store case.e01") {
		t.Fatalf("ctrl+c while a store runs: quit %v, dialog %d; screen:\n%s", d.quit, d.m.quit, d.view())
	}
	d.press("enter") // No is chosen
	if d.m.quit != quitNone || !d.jobs.busy() || g.ended() != nil {
		t.Fatalf("enter on No: dialog %d, store running %v (ended %v)", d.m.quit, d.jobs.busy(), g.ended())
	}

	d.press("ctrl+c", "y")
	if d.m.quit != quitStopping || !d.shows("Waiting for: store case.e01") {
		t.Fatalf("y: dialog %d; screen:\n%s", d.m.quit, d.view())
	}
	d.until("the program quits", func() bool { return d.quit })
	if !errors.Is(g.ended(), errStopping) {
		t.Fatalf("the store ended with %v, want %v", g.ended(), errStopping)
	}
	if d.jobs.busy() {
		t.Fatal("the program quit before the store stopped")
	}
}

// TestQuitDialogKeys: n, esc and q keep working; tab and the arrows change
// the choice; enter takes it.
func TestQuitDialogKeys(t *testing.T) {
	g := newGate()
	d := newDriver(t, Actions{Store: storeWith(g, Outcome{})})
	d.open(StateStore)
	d.typeText("a")
	d.press("enter")
	g.waitStarted(t)
	d.press("esc") // the store keeps running
	for _, k := range []string{"n", "esc", "q"} {
		d.press("q") // on the menu, q asks too
		if d.m.quit != quitAsking {
			t.Fatalf("q on the menu with a job running: dialog %d", d.m.quit)
		}
		d.press(k)
		if d.m.quit != quitNone || d.quit {
			t.Fatalf("%s in the dialog: dialog %d, quit %v", k, d.m.quit, d.quit)
		}
	}
	d.press("ctrl+c", "tab", "right")
	if d.m.quitYes {
		t.Fatal("tab then right: Yes chosen, want No")
	}
	d.press("left", "enter")
	if d.m.quit != quitStopping {
		t.Fatalf("enter on Yes: dialog %d", d.m.quit)
	}
	d.until("the program quits", func() bool { return d.quit })
}

// TestSecondCtrlCWhileStoppingQuits: while stopping, ctrl+c quits at once.
// Run still waits for the job before the database is closed.
func TestSecondCtrlCWhileStoppingQuits(t *testing.T) {
	g := newGate()
	g.ignoreCancel = true
	d := newDriver(t, Actions{Store: storeWith(g, Outcome{})})
	t.Cleanup(func() { close(g.release) })
	d.open(StateStore)
	d.typeText("a")
	d.press("enter")
	g.waitStarted(t)
	d.press("ctrl+c", "y")
	d.press("q", "esc", "n")
	d.settle()
	if d.quit || d.m.quit != quitStopping {
		t.Fatalf("keys while stopping: quit %v, dialog %d", d.quit, d.m.quit)
	}
	d.press("ctrl+c")
	d.until("the program quits", func() bool { return d.quit })
}

// TestQuitWithNothingRunning: q and ctrl+c on the menu quit at once when no
// job runs.
func TestQuitWithNothingRunning(t *testing.T) {
	for _, k := range []string{"q", "esc", "ctrl+c"} {
		d := newDriver(t, Actions{})
		d.press(k)
		d.until("the program quits on "+k, func() bool { return d.quit })
	}
	d := newDriver(t, Actions{})
	d.open(StateSearch)
	d.press("ctrl+c")
	d.until("the program quits on ctrl+c on a screen", func() bool { return d.quit })
}

// TestQIsText (TUI-4): q is typed into a focused input; elsewhere it goes
// back to the menu.
func TestQIsText(t *testing.T) {
	cases := []struct {
		state State
		setup []string
		value func(*model) string
		blur  []string // moves the focus off the input
	}{
		{StateStore, nil, func(m *model) string { return m.screen.(*storeScreen).path.Value() }, []string{"tab"}},
		{StateSearch, nil, func(m *model) string { return m.screen.(*searchScreen).query.Value() }, nil},
		{StateRestore, nil, func(m *model) string { return m.screen.(*restoreScreen).id.Value() }, []string{"tab", "tab"}},
		{StateNear, []string{"enter"}, func(m *model) string { return m.screen.(*nearScreen).input.Value() }, []string{"tab"}},
		{StateReset, nil, func(m *model) string { return m.screen.(*resetScreen).input.Value() }, []string{"tab"}},
	}
	for _, c := range cases {
		d := newDriver(t, Actions{})
		d.open(c.state)
		d.press(c.setup...)
		d.typeText("aqq")
		d.settle()
		if d.m.state != c.state || c.value(d.m) != "aqq" {
			t.Fatalf("screen %d: typing aqq left state %d, input %q", c.state, d.m.state, c.value(d.m))
		}
		if c.blur == nil {
			continue // the search input always has focus
		}
		d.press(c.blur...)
		d.press("q")
		if d.m.state != StateMenu || d.quit {
			t.Fatalf("screen %d: q off the input: state %d, quit %v", c.state, d.m.state, d.quit)
		}
	}
}

// TestNearOptionsAreCheckboxes (TUI-4): Deep and Advanced deep are
// checkboxes in both modes, typing d into the query is text, and the
// analysis gets what is ticked.
func TestNearOptionsAreCheckboxes(t *testing.T) {
	type call struct {
		mode, query string
		opts        NearOptions
	}
	var calls record[call]
	near := func(mode string) func(context.Context, string, NearOptions) (Outcome, error) {
		return func(_ context.Context, query string, o NearOptions) (Outcome, error) {
			calls.add(call{mode, query, o})
			return Outcome{Lines: []string{"Matched " + query}}, nil
		}
	}
	d := newDriver(t, Actions{NearIn: near(nearIn), NearOut: near(nearOut)})
	d.open(StateNear)
	d.press("enter") // a stored object
	d.typeText("dd")
	s := d.m.screen.(*nearScreen)
	if s.opts.Deep || s.input.Value() != "dd" {
		t.Fatalf("typing dd: deep %v, input %q", s.opts.Deep, s.input.Value())
	}
	d.press("tab", "space")
	if !s.opts.Deep || !d.shows("☑ Deep") {
		t.Fatalf("space on the checkbox: deep %v", s.opts.Deep)
	}
	d.press("enter", "enter")
	if !s.opts.Deep || s.opts.Advanced {
		t.Fatalf("enter twice on the checkbox: %+v", s.opts)
	}
	d.press("tab", "tab", "enter") // past Advanced deep to Analyse
	d.until("the analysis ends", func() bool { return calls.len() == 1 && !s.run.active() })
	if c := calls.get()[0]; c != (call{nearIn, "dd", NearOptions{Deep: true}}) || !d.shows("Matched dd") {
		t.Fatalf("near in got %+v; screen:\n%s", c, d.view())
	}

	d.press("esc", "down", "enter") // a file outside the database
	if s.mode != nearOut || !d.shows("☑ Deep") || !d.shows("☐ Advanced deep") {
		t.Fatalf("near out: mode %q; screen:\n%s", s.mode, d.view())
	}
	d.typeText("x.bin")
	d.press("tab", "space", "tab", "space", "tab", "enter")
	d.until("the analysis ends", func() bool { return calls.len() == 2 && !s.run.active() })
	if c := calls.get()[1]; c != (call{nearOut, "x.bin", NearOptions{Advanced: true}}) {
		t.Fatalf("near out got %+v", c)
	}
}

// TestNearModeListCannotPanic (TUI-4): the mode list has no filter, and an
// empty selection is not taken for a mode.
func TestNearModeListCannotPanic(t *testing.T) {
	d := newDriver(t, Actions{})
	d.open(StateNear)
	d.press("/", "z", "z")
	d.settle()
	s := d.m.screen.(*nearScreen)
	s.modes.SetItems(nil)
	d.press("enter")
	if s.mode != "" || d.m.fatal != nil {
		t.Fatalf("enter on an empty mode list: mode %q, fatal %v", s.mode, d.m.fatal)
	}
	d.press("esc")
	if d.m.state != StateMenu {
		t.Fatalf("esc on the mode list: state %d", d.m.state)
	}
}

// TestCancelReturnsToMenu (TUI-4): the Cancel buttons go back to the menu;
// they used to quit.
func TestCancelReturnsToMenu(t *testing.T) {
	for _, c := range []struct {
		state State
		tabs  int
	}{{StateStore, storeFocusCancel}, {StateRestore, 3}} {
		d := newDriver(t, Actions{})
		d.open(c.state)
		for range c.tabs {
			d.press("tab")
		}
		d.press("enter")
		d.until("back on the menu", func() bool { return d.m.state == StateMenu })
		d.settle()
		if d.quit {
			t.Fatalf("screen %d: Cancel quit the program", c.state)
		}
	}
}

// TestEscKeepsTheJob (TUI-4): leaving a screen leaves its job running, the
// menu lists it, ctrl+x on the menu cancels it, and its outcome shows.
func TestEscKeepsTheJob(t *testing.T) {
	var mu sync.Mutex
	var causes []error
	d := newDriver(t, Actions{Search: func(ctx context.Context, q string) (Outcome, error) {
		<-ctx.Done()
		mu.Lock()
		causes = append(causes, context.Cause(ctx))
		mu.Unlock()
		return Outcome{}, ctx.Err()
	}})
	d.open(StateSearch)
	d.typeText("ab")
	d.press("enter")
	d.until("the search runs", d.jobs.busy)
	d.press("esc")
	if d.m.state != StateMenu || !d.jobs.busy() || !d.shows("Running: search ab") {
		t.Fatalf("esc: state %d, running %v; screen:\n%s", d.m.state, d.jobs.running(), d.view())
	}
	d.press("ctrl+x")
	d.until("the search ends", func() bool { return !d.jobs.busy() && d.shows("search ab: cancelled") })
	if len(causes) != 1 || !errors.Is(causes[0], errCanceled) {
		t.Fatalf("the search ended with %v", causes)
	}
}

// TestCtrlXCancelsTheScreensJob: on a screen, ctrl+x cancels its own job
// only.
func TestCtrlXCancelsTheScreensJob(t *testing.T) {
	store, search := newGate(), newGate()
	d := newDriver(t, Actions{
		Store:  storeWith(store, Outcome{}),
		Search: func(ctx context.Context, _ string) (Outcome, error) { return Outcome{}, search.wait(ctx) },
	})
	d.open(StateStore)
	d.typeText("a")
	d.press("enter")
	store.waitStarted(t)
	d.press("esc")
	d.open(StateSearch)
	d.typeText("ab")
	d.press("enter")
	search.waitStarted(t)
	if !d.shows("Running: store a") || d.shows("Running: search ab") {
		t.Fatalf("footer: want the store only; screen:\n%s", d.view())
	}
	d.press("ctrl+x")
	s := d.m.screen.(*searchScreen)
	d.until("the search ends", func() bool { return !s.run.active() })
	if !d.shows("Error: cancelled") || store.ended() != nil || len(d.jobs.running()) != 1 {
		t.Fatalf("ctrl+x: store ended %v, running %v; screen:\n%s", store.ended(), d.jobs.running(), d.view())
	}
}

// TestOutcomeAfterLeaving: a store that ends after the screen was left
// reports on the menu, warnings as warnings.
func TestOutcomeAfterLeaving(t *testing.T) {
	g := newGate()
	d := newDriver(t, Actions{Store: storeWith(g, Outcome{Lines: []string{"Stored as evidence AAA (10 bytes, raw)", "Index partial"}, Warning: true})})
	d.open(StateStore)
	d.typeText("a.img")
	d.press("enter")
	g.waitStarted(t)
	d.press("esc")
	close(g.release)
	d.until("the store ends", func() bool { return !d.jobs.busy() && d.m.status.text != "" })
	if d.m.status.kind != statusWarn || !d.shows("store a.img: Stored as evidence AAA (10 bytes, raw) (with warnings)") {
		t.Fatalf("status %+v; screen:\n%s", d.m.status, d.view())
	}
}

// TestStoreScreen: the outcome's lines are shown, warnings as warnings; the
// layout is shown read-only and the store gets only its options (CFG-6);
// "store another" starts afresh.
func TestStoreScreen(t *testing.T) {
	type call struct {
		path string
		opts StoreOptions
	}
	var calls record[call]
	d := newDriver(t, Actions{Store: func(_ context.Context, path string, o StoreOptions) (Outcome, error) {
		calls.add(call{path, o})
		if path == "bad" {
			return Outcome{}, errors.New("open bad: no such file")
		}
		return Outcome{Lines: []string{"Stored as evidence AAA", "Warning: 2 EWF chunk tables failed"}, Warning: true}, nil
	}})
	d.open(StateStore)
	if !d.shows("Chunks of 256 KiB, hashed with blake3: the database's own layout") || d.shows("SHA3") {
		t.Fatalf("store screen:\n%s", d.view())
	}
	d.press("enter")
	if !d.shows("Error: enter the path") || calls.len() != 0 {
		t.Fatalf("enter with no path: %v", calls.get())
	}
	d.typeText(" a b ")
	// Skip the index, then past reverse relations and chunk signatures; the
	// full-text index and enrichment need the index, so focus skips them.
	d.press("tab", "space", "tab", "tab", "tab", "enter")
	s := d.m.screen.(*storeScreen)
	d.until("the store ends", func() bool { return calls.len() == 1 && !s.run.active() })
	if c := calls.get()[0]; c.path != "a b" || c.opts != (StoreOptions{NoIndex: true}) {
		t.Fatalf("store got %+v", c)
	}
	if s.status.kind != statusWarn || !d.shows("Warning: 2 EWF chunk tables failed") || !d.shows("Store another file or folder?") {
		t.Fatalf("after a store with warnings; screen:\n%s", d.view())
	}
	d.press("enter") // Yes is chosen
	if !s.typing() || s.path.Value() != "" || d.shows("Warning: 2 EWF") {
		t.Fatalf("store another: typing %v, path %q", s.typing(), s.path.Value())
	}
	d.typeText("bad")
	d.press("enter")
	d.until("the store ends", func() bool { return calls.len() == 2 && !s.run.active() })
	if !d.shows("Error: open bad: no such file") || d.shows("Store another") {
		t.Fatalf("a failed store; screen:\n%s", d.view())
	}
}

// TestStoreOptions: the store gets the options ticked. The full-text index
// and enrichment read the index, so skipping it turns them off, and the
// focus passes them by while they cannot be chosen.
func TestStoreOptions(t *testing.T) {
	var got record[StoreOptions]
	d := newDriver(t, Actions{Store: func(_ context.Context, _ string, o StoreOptions) (Outcome, error) {
		got.add(o)
		return Outcome{Lines: []string{"Stored as evidence AAA"}}, nil
	}})
	d.open(StateStore)
	s := d.m.screen.(*storeScreen)
	d.typeText("ev.bin")
	d.press("tab", "tab", "space", "tab", "space", "tab", "space", "tab", "space", "tab", "enter")
	d.until("the store ends", func() bool { return got.len() == 1 && !s.run.active() })
	if o := got.get()[0]; o != (StoreOptions{RevRel: true, Simhash: true, FTS: true, Enrich: true}) {
		t.Fatalf("store got %+v", o)
	}

	d.press("enter") // store another
	d.typeText("ev2.bin")
	d.press("tab", "space")
	if s.opts[optFTS] || s.opts[optEnrich] || !d.shows("☐ Full-text index: for `dues search --fts` (needs the index)") {
		t.Fatalf("skipping the index left %v; screen:\n%s", s.opts, d.view())
	}
	d.press("tab", "tab", "tab")
	if s.focus != storeFocusStore {
		t.Fatalf("focus %d after the options, want the Store button", s.focus)
	}
	d.press("shift+tab")
	if s.focus != storeFocusOption(optSimhash) {
		t.Fatalf("focus %d going back, want chunk signatures", s.focus)
	}
	d.press("tab", "enter")
	d.until("the store ends", func() bool { return got.len() == 2 && !s.run.active() })
	if o := got.get()[1]; o != (StoreOptions{NoIndex: true, RevRel: true, Simhash: true}) {
		t.Fatalf("second store got %+v", o)
	}
}

// TestStoreQuestionFits: after a store on an 80 by 24 terminal, the report
// and the question whether to store another are on screen; the options give
// way to them.
func TestStoreQuestionFits(t *testing.T) {
	d := newDriver(t, Actions{Store: func(context.Context, string, StoreOptions) (Outcome, error) {
		return Outcome{Lines: []string{"Storing ev.bin", "Stored as evidence AAA (16 bytes, raw) in 1s",
			"Index complete: 1 partitions, 3 of 3 files indexed", "Warning: 2 EWF chunk tables failed"}, Warning: true}, nil
	}})
	d.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	d.open(StateStore)
	s := d.m.screen.(*storeScreen)
	d.typeText("ev.bin")
	d.press("enter")
	d.until("the store ends", func() bool { return !s.run.active() && s.another.open })
	view := d.view()
	if n := strings.Count(view, "\n") + 1; n > 24 || !d.shows("Store another file or folder?") || !d.shows("Warning: 2 EWF") {
		t.Fatalf("%d lines; screen:\n%s", n, view)
	}
}

// TestRestoreAsksBeforeReplacing (TUI-2): an existing target is replaced
// only after the user agrees; No is chosen.
func TestRestoreAsksBeforeReplacing(t *testing.T) {
	type call struct {
		id, path string
		replace  bool
	}
	var calls record[call]
	restore := func(_ context.Context, id, path string, replace bool) (Outcome, error) {
		calls.add(call{id, path, replace})
		if !replace {
			return Outcome{}, ErrTargetExists
		}
		return Outcome{Lines: []string{"Restored to " + path}}, nil
	}
	d := newDriver(t, Actions{Restore: restore})
	d.open(StateRestore)
	d.typeText("AAAA")
	d.press("enter") // to the path
	d.typeText("out.bin")
	d.press("enter")
	s := d.m.screen.(*restoreScreen)
	d.until("asked to replace", func() bool { return s.replace.open })
	if !d.shows("out.bin exists. Replace it?") {
		t.Fatalf("screen:\n%s", d.view())
	}
	d.press("enter") // No
	if s.replace.open || calls.len() != 1 || !d.shows("Not restored: out.bin exists") {
		t.Fatalf("No: calls %v; screen:\n%s", calls.get(), d.view())
	}
	d.press("tab", "enter") // Restore button
	d.until("asked again", func() bool { return s.replace.open })
	d.press("esc")
	if s.replace.open || d.m.state != StateRestore {
		t.Fatalf("esc on the prompt: open %v, state %d", s.replace.open, d.m.state)
	}
	d.press("enter")
	d.until("asked again", func() bool { return s.replace.open })
	d.press("y")
	d.until("restored", func() bool { return calls.len() == 4 && !s.run.active() })
	if c := calls.get()[3]; c != (call{"AAAA", "out.bin", true}) || !d.shows("Restored to out.bin") {
		t.Fatalf("calls %v; screen:\n%s", calls.get(), d.view())
	}
}

func evidenceRows(n int) []EvidenceRow {
	rows := make([]EvidenceRow, n)
	for i := range rows {
		rows[i] = EvidenceRow{ID: fmt.Sprintf("ID%05d", i), Name: fmt.Sprintf("evidence-%d.img", i), Size: int64(i), Format: "raw", Index: "complete"}
	}
	return rows
}

// TestListWindowFollowsTheSelection (TUI-5): the list is drawn as a window
// around the selection, and says when it holds only the first rows.
func TestListWindowFollowsTheSelection(t *testing.T) {
	var limit int
	d := newDriver(t, Actions{List: func(_ context.Context, n int) (EvidenceList, error) {
		limit = n
		return EvidenceList{Rows: evidenceRows(1000), Total: 1200}, nil
	}})
	d.open(StateList)
	s := d.m.screen.(*listScreen)
	d.until("the list loads", func() bool { return s.loaded })
	if limit != listLimit || !d.shows("The first 1,000 of 1,200 evidence; `dues list` lists them all.") {
		t.Fatalf("limit %d; screen:\n%s", limit, d.view())
	}
	for range 500 {
		d.press("down")
	}
	if !d.shows("ID00500") || !d.shows("501 of 1000") {
		t.Fatalf("row 500 is not on screen:\n%s", d.view())
	}
	d.press("pgdown", "G")
	if s.sel != 999 || !d.shows("ID00999") {
		t.Fatalf("G: selection %d", s.sel)
	}
	d.press("g", "up", "k")
	if s.sel != 0 || !d.shows("ID00000") {
		t.Fatalf("g: selection %d", s.sel)
	}
	if lines := strings.Count(d.view(), "\n"); lines > d.m.env.height+2 {
		t.Fatalf("the list draws %d lines in a %d-line terminal", lines, d.m.env.height)
	}
	d.press("q")
	if d.m.state != StateMenu {
		t.Fatalf("q on the list: state %d", d.m.state)
	}
}

// TestListRestore: the list restores the selected evidence beside the working
// directory, asking before it replaces a file.
func TestListRestore(t *testing.T) {
	var replaced record[bool]
	d := newDriver(t, Actions{
		List: func(context.Context, int) (EvidenceList, error) {
			return EvidenceList{Rows: []EvidenceRow{{ID: "AQID/+==", Name: "a"}}, Total: 1}, nil
		},
		Restore: func(_ context.Context, id, path string, replace bool) (Outcome, error) {
			replaced.add(replace)
			if path != "restored_AQID_-.bin" || id != "AQID/+==" {
				return Outcome{}, fmt.Errorf("restore %s to %s", id, path)
			}
			if !replace {
				return Outcome{}, ErrTargetExists
			}
			return Outcome{}, nil
		},
	})
	d.open(StateList)
	s := d.m.screen.(*listScreen)
	d.until("the list loads", func() bool { return s.loaded })
	d.press("r")
	d.until("asked to replace", func() bool { return s.replace.open })
	d.press("esc")
	if d.m.state != StateList || s.replace.open {
		t.Fatalf("esc on the prompt: state %d", d.m.state)
	}
	d.press("enter")
	d.until("asked to replace", func() bool { return s.replace.open })
	d.press("y")
	d.until("restored", func() bool { return replaced.len() == 3 && !s.run.active() })
	if !replaced.get()[2] || !d.shows("Restored to restored_AQID_-.bin") {
		t.Fatalf("replace %v; screen:\n%s", replaced.get(), d.view())
	}
}

// TestClipboardIsACommand (TUI-5): Update does not write the clipboard; the
// command it returns does.
func TestClipboardIsACommand(t *testing.T) {
	var written []string
	saved := writeClipboard
	writeClipboard = func(s string) error { written = append(written, s); return nil }
	t.Cleanup(func() { writeClipboard = saved })

	d := newDriver(t, Actions{List: func(context.Context, int) (EvidenceList, error) {
		return EvidenceList{Rows: evidenceRows(3), Total: 3}, nil
	}})
	d.open(StateList)
	s := d.m.screen.(*listScreen)
	d.until("the list loads", func() bool { return s.loaded })
	d.press("down")
	_, cmd := d.m.Update(key("c"))
	if len(written) != 0 || cmd == nil {
		t.Fatalf("Update wrote the clipboard: %v", written)
	}
	d.handle(cmd())
	if len(written) != 1 || written[0] != "ID00001" || !d.shows("Copied ID00001") {
		t.Fatalf("written %v; screen:\n%s", written, d.view())
	}
}

// TestEvidenceStringsAreMadeSafe (TUI-5): an escape sequence in a name, an
// outcome or an error does not reach the terminal.
func TestEvidenceStringsAreMadeSafe(t *testing.T) {
	const evil = "x\x1b]0;pwned\x07\x1b[2J"
	tree := microTree(evil, evil, evil, evil, evil, 1)
	acts := Actions{
		List: func(context.Context, int) (EvidenceList, error) {
			return EvidenceList{Rows: []EvidenceRow{{ID: evil, Name: evil, Format: evil, Index: evil}}, Total: 1}, nil
		},
		Micro: func(context.Context) (MicroView, error) {
			return MicroView{Tree: tree, Files: 1, Artefacts: 1, Shown: 1, PerFile: 20}, nil
		},
		Search: func(context.Context, string) (Outcome, error) {
			return Outcome{Lines: []string{evil}}, errors.New(evil)
		},
		Store: func(context.Context, string, StoreOptions) (Outcome, error) {
			return Outcome{Lines: []string{evil}}, nil
		},
	}
	check := func(where string, d *driver) {
		t.Helper()
		if v := d.view(); strings.Contains(v, "\x1b]0;") || strings.Contains(v, "\x07") || strings.Contains(v, "\x1b[2J") {
			t.Fatalf("%s: the escape sequence reached the screen: %q", where, v)
		}
	}
	d := newDriver(t, acts)
	d.m.env.info.Root = evil
	check("menu", d)

	d.open(StateList)
	d.until("the list loads", func() bool { return d.m.screen.(*listScreen).loaded })
	check("list", d)
	d.press("esc")

	d.open(StateMicro)
	d.until("the view loads", func() bool { return d.m.screen.(*microScreen).loaded })
	check("micro", d)
	d.press("esc")

	d.open(StateSearch)
	d.typeText("ab")
	d.press("enter")
	d.until("the search ends", func() bool { return !d.jobs.busy() })
	check("search", d)
	d.press("esc")
	check("menu status", d)

	d.open(StateStore)
	d.typeText("a")
	d.press("enter")
	d.until("the store ends", func() bool { return !d.jobs.busy() })
	check("store", d)
}

// TestResetNeedsTheNameAndDefaultsToNo (TUI-1): enter never resets from the
// name field; No is the next button; Yes works only once the data root's
// name is typed.
func TestResetNeedsTheNameAndDefaultsToNo(t *testing.T) {
	var resets atomic.Int32
	d := newDriver(t, Actions{Reset: func(context.Context) error { resets.Add(1); return nil }})
	d.open(StateReset)
	if !d.shows("Type db to confirm") {
		t.Fatalf("reset screen:\n%s", d.view())
	}
	d.press("enter")
	s := d.m.screen.(*resetScreen)
	if s.focus != resetFocusNo {
		t.Fatalf("enter in the field: focus %d, want No", s.focus)
	}
	d.press("enter")
	d.until("back on the menu", func() bool { return d.m.state == StateMenu })

	d.open(StateReset)
	s = d.m.screen.(*resetScreen)
	d.typeText("dbx")
	d.press("enter", "tab", "enter") // to No, to Yes, press
	d.settle()
	if resets.Load() != 0 || !d.shows("Type db to confirm.") {
		t.Fatalf("Yes with the wrong name: %d resets; screen:\n%s", resets.Load(), d.view())
	}
	d.press("tab", "backspace", "enter", "tab", "enter") // the field again: "db"
	d.until("the reset ends", func() bool { return s.done })
	if resets.Load() != 1 || !d.shows("The database is empty.") {
		t.Fatalf("Yes with the name: %d resets; screen:\n%s", resets.Load(), d.view())
	}
	d.press("enter")
	d.until("back on the menu", func() bool { return d.m.state == StateMenu })
}

// TestResetWaitsForOtherJobs: the reset is unavailable while another job
// runs, and nothing starts while it runs.
func TestResetWaitsForOtherJobs(t *testing.T) {
	store, reset := newGate(), newGate()
	d := newDriver(t, Actions{
		Store: storeWith(store, Outcome{}),
		Reset: func(ctx context.Context) error { return reset.wait(ctx) },
	})
	d.open(StateStore)
	d.typeText("a")
	d.press("enter")
	store.waitStarted(t)
	d.press("esc")
	d.open(StateReset)
	d.typeText("db")
	d.press("enter", "tab", "enter")
	d.settle()
	if reset.count() != 0 || !d.shows("A reset cannot run while other jobs run (store a)") {
		t.Fatalf("reset beside a store: %d resets; screen:\n%s", reset.count(), d.view())
	}
	close(store.release)
	d.until("the store ends", func() bool { return !d.jobs.busy() })
	d.press("enter")
	reset.waitStarted(t)

	d.press("esc")
	d.open(StateStore)
	d.typeText("b")
	d.press("enter")
	if !d.shows("a reset is running (reset)") || store.count() != 1 {
		t.Fatalf("a store beside a reset; screen:\n%s", d.view())
	}
	close(reset.release)
	d.until("the reset ends", func() bool { return !d.jobs.busy() })
}

// TestStatsScreen: the layout comes from the database's own description;
// a loader's failure shows on the menu, its success does not.
func TestStatsScreen(t *testing.T) {
	fail := true
	d := newDriver(t, Actions{Stats: func(context.Context) (*store.DBStats, error) {
		if fail {
			return nil, errors.New("invalid key")
		}
		return &store.DBStats{TotalFiles: 2, CompletedFiles: 2, TotalLogicalSize: 2048, UniqueChunks: 3, TotalChunkRefs: 4}, nil
	}})
	d.open(StateStats)
	d.until("the statistics fail", func() bool { return d.shows("Error: invalid key") })
	d.press("esc")
	if !d.shows("statistics: invalid key") {
		t.Fatalf("menu after a failed loader:\n%s", d.view())
	}
	fail = false
	d.m.status = status{}
	d.open(StateStats)
	d.until("the statistics load", func() bool { return d.m.screen.(*statsScreen).stats != nil })
	text := d.m.screen.(*statsScreen).render()
	for _, want := range []string{"256 KiB", "blake3", "Deduplication rate", "Encryption"} {
		if !strings.Contains(text, want) {
			t.Fatalf("statistics lack %q:\n%s", want, text)
		}
	}
	d.press("esc")
	if d.m.status.text != "" {
		t.Fatalf("a loader's success set the menu status: %q", d.m.status.text)
	}
}

// microTree is one evidence, partition and file holding one artefact of
// count.
func microTree(evidence, partition, path, kind, value string, count int) *microartefact.EvidenceFileHierarchy {
	a := &microartefact.MicroArtefactNode{}
	a.Kind, a.Value = kind, value
	f := &microartefact.IndexedFileArtefactView{Artefacts: []*microartefact.MicroArtefactNode{a}, ArtefactCount: count}
	f.Path = path
	tree := &microartefact.EvidenceFileHierarchy{}
	tree.EvidenceFiles = []*microartefact.EvidenceFileNode{{Name: evidence, Partitions: []*microartefact.PartitionFileNode{
		{Name: partition, Files: []*microartefact.IndexedFileArtefactView{f}},
	}}}
	return tree
}

// TestMicroScreen: the view says when it shows only some files, and how many
// artefacts a file holds beyond those shown.
func TestMicroScreen(t *testing.T) {
	tree := microTree("case.e01", "p1", "/Windows/notepad.exe", "url", "http://example.com/"+strings.Repeat("a", 200), 25)
	d := newDriver(t, Actions{Micro: func(context.Context) (MicroView, error) {
		return MicroView{Tree: tree, Files: 700, Artefacts: 9000, Shown: 500, PerFile: 20}, nil
	}})
	d.open(StateMicro)
	d.until("the view loads", func() bool { return d.m.screen.(*microScreen).loaded })
	if !d.shows("The 500 files with the most artefacts, of 700 with 9000") {
		t.Fatalf("micro view:\n%s", d.view())
	}
	text := d.m.screen.(*microScreen).render()
	for _, want := range []string{"notepad.exe (0 bytes, 25 artefacts)", "… and 24 more", strings.Repeat("a", 80) + "…\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("micro view lacks %q:\n%s", want, text)
		}
	}

	empty := newDriver(t, Actions{Micro: func(context.Context) (MicroView, error) { return MicroView{}, nil }})
	empty.open(StateMicro)
	empty.until("the view loads", func() bool { return empty.m.screen.(*microScreen).loaded })
	if !empty.shows("No file has micro-artefacts") {
		t.Fatalf("empty view:\n%s", empty.view())
	}
}

// TestMenuShowsTheDatabase (CFG-6): the menu shows the layout read-only and
// says when the database is not encrypted.
func TestMenuShowsTheDatabase(t *testing.T) {
	d := newDriver(t, Actions{})
	for _, want := range []string{testInfo.Root, "Chunks of 256 KiB, hashed with blake3", "This database is not encrypted."} {
		if !d.shows(want) {
			t.Fatalf("menu lacks %q:\n%s", want, d.view())
		}
	}
	d.m.env.info.Encrypted = true
	if d.shows("not encrypted") {
		t.Fatal("an encrypted database is shown as not encrypted")
	}
}

// panicScreen panics when it handles a message or draws.
type panicScreen struct{ inView bool }

func (panicScreen) init() tea.Cmd          { return nil }
func (panicScreen) resize()                {}
func (panicScreen) typing() bool           { return false }
func (panicScreen) job() int               { return 0 }
func (panicScreen) update(tea.Msg) tea.Cmd { panic("update") }
func (p panicScreen) view() string {
	if p.inView {
		panic("view")
	}
	return ""
}

type unknownMsg struct{}

// TestModelPanicIsContained: a panic in Update ends the program and cancels
// every job; one in View is drawn, and the next key ends the program.
func TestModelPanicIsContained(t *testing.T) {
	g := newGate()
	d := newDriver(t, Actions{Store: storeWith(g, Outcome{})})
	d.open(StateStore)
	d.typeText("a")
	d.press("enter")
	g.waitStarted(t)
	d.m.screen, d.m.state = panicScreen{}, StateStore
	d.send(unknownMsg{})
	d.until("the program quits and the store stops", func() bool { return d.quit && !d.jobs.busy() })
	if d.m.fatal == nil || !errors.Is(g.ended(), errStopping) {
		t.Fatalf("fatal %v; the store ended with %v", d.m.fatal, g.ended())
	}

	v := newDriver(t, Actions{})
	v.m.screen, v.m.state = panicScreen{inView: true}, StateStore
	if !v.shows("Internal error: internal error (panic ") {
		t.Fatalf("view after a panic:\n%s", v.view())
	}
	v.press("x")
	v.until("the program quits", func() bool { return v.quit })
}
