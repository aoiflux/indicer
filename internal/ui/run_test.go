package ui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"indicer/internal/progress"
	"indicer/internal/testutil/vtscreen"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// harness runs a display against a screen: out and errw are what
// console.Out and console.Err become while it runs.
type harness struct {
	t         *testing.T
	clock     *fakeClock
	scr       *screen
	plain     *bytes.Buffer // stderr in plain mode
	stdout    *bytes.Buffer // a piped stdout
	out, errw io.Writer
	restored  bool
	rn        *runner
	ctx       context.Context
}

func newHarness(t *testing.T, mode renderMode, width, height int, shared bool) *harness {
	return newHarnessWith(t, mode, width, height, shared, false)
}

// newTaskbarHarness is a live harness on a terminal that shows progress on
// its tab, from a second in.
func newTaskbarHarness(t *testing.T) *harness {
	return newHarnessWith(t, renderLive, 100, 24, true, true)
}

func newHarnessWith(t *testing.T, mode renderMode, width, height int, shared, taskbar bool) *harness {
	h := &harness{t: t, clock: &fakeClock{t: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}, stdout: new(bytes.Buffer), plain: new(bytes.Buffer)}
	var opts []vtscreen.Option
	if taskbar {
		opts = append(opts, vtscreen.AllowTaskbar())
	}
	h.scr = newScreen(t, width, opts...)
	var progressW io.Writer = h.scr
	var errRaw io.Writer = h.scr
	outRaw := io.Writer(h.stdout)
	if mode == renderPlain {
		progressW, errRaw = h.plain, h.plain
	}
	if shared {
		outRaw = h.scr
	}
	cfg := runConfig{
		mode: mode, now: h.clock.now, tick: 100 * time.Millisecond, heartbeat: 15 * time.Second, hold: 250 * time.Millisecond,
		size:     func() (int, int) { return width, height },
		painter:  Painter{glyphs: GlyphsFull, decorated: mode == renderLive},
		progress: progressW, shared: shared, manual: true, taskbar: taskbar, taskbarDelay: time.Second,
		wrap: func(f func(out, err io.Writer) (io.Writer, io.Writer)) func() {
			h.out, h.errw = f(outRaw, errRaw)
			return func() { h.restored = true }
		},
	}
	if mode == renderPlain {
		cfg.painter = Painter{glyphs: GlyphsASCII}
	}
	rep := progress.New(context.Background(), h.clock.now)
	h.rn = newRunner(cfg, rep)
	h.ctx = progress.With(context.Background(), rep)
	h.rn.start()
	return h
}

func (h *harness) step(d time.Duration) {
	h.clock.add(d)
	h.rn.step()
}

func (h *harness) screenHas(want ...string) {
	h.t.Helper()
	got := h.scr.String()
	for _, w := range want {
		if !strings.Contains(got, w) {
			h.t.Errorf("screen lacks %q:\n%s", w, got)
		}
	}
}

func TestLiveSinglePhase(t *testing.T) {
	h := newHarness(t, renderLive, 100, 24, false)
	p := progress.Start(h.ctx, "store", progress.Bytes, 4<<30)
	read := progress.Tally(h.ctx, "read", progress.Bytes)
	fresh := progress.Tally(h.ctx, "new", progress.Bytes)
	for range 3 {
		p.Add(1 << 30)
		read.Add(1 << 30)
		fresh.Add(1 << 29)
		h.step(time.Second)
	}
	h.screenHas("storing", "75%", "3.00/4.00 GiB", "ETA")
	if !h.scr.Hidden() {
		t.Error("cursor not hidden while drawing")
	}
	p.Add(1 << 30)
	read.Add(1 << 30)
	p.Done(nil)
	h.clock.add(time.Second)
	h.rn.stop(nil)
	lines := h.scr.Text()
	if len(lines) != 2 {
		t.Fatalf("final screen:\n%s", h.scr)
	}
	if !strings.HasPrefix(lines[0], "  ✓ store") || !strings.Contains(lines[0], "4.00 GiB") {
		t.Errorf("final row %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "✓ done in 4.0s") || !strings.Contains(lines[1], "read 4.00 GiB") ||
		!strings.Contains(lines[1], "new 1.50 GiB") || !strings.Contains(lines[1], "deduplicated 2.50 GiB (62%)") {
		t.Errorf("footer %q", lines[1])
	}
	if h.scr.Hidden() || !h.restored {
		t.Error("cursor left hidden or sinks not restored")
	}
}

func TestLiveFailureNeverShowsFull(t *testing.T) {
	h := newHarness(t, renderLive, 100, 24, false)
	p := progress.Start(h.ctx, "store", progress.Bytes, 100)
	p.Add(100)
	h.step(time.Second)
	if strings.Contains(h.scr.String(), "100%") {
		t.Errorf("a running bar shows 100%%:\n%s", h.scr)
	}
	p.Done(errors.New("disk full"))
	h.rn.stop(errors.New("disk full"))
	h.screenHas("✗ store", "failed at 99% (100/100 B): disk full", "✗ failed after")
	if strings.Contains(h.scr.String(), "100%") {
		t.Errorf("a failed phase shows 100%%:\n%s", h.scr)
	}
}

func TestLiveWritesGoAboveTheRegion(t *testing.T) {
	h := newHarness(t, renderLive, 80, 24, true)
	p := progress.Start(h.ctx, "index", progress.Files, progress.Unknown)
	p.Add(10)
	h.step(time.Second)
	fmt.Fprintln(h.out, "Storing a.dd")
	fmt.Fprint(h.errw, "dues: note: ")
	fmt.Fprintln(h.errw, "half a line, then the rest")
	p.Add(5)
	h.step(time.Second)
	fmt.Fprintln(h.out, "Stored as evidence X")
	p.Done(nil)
	h.rn.stop(nil)
	want := []string{"Storing a.dd", "dues: note: half a line, then the rest", "Stored as evidence X"}
	lines := h.scr.Text()
	if len(lines) < 5 {
		t.Fatalf("screen:\n%s", h.scr)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("line %d = %q, want %q\n%s", i, lines[i], w, h.scr)
		}
	}
	if !strings.HasPrefix(lines[3], "  ✓ index") || !strings.HasPrefix(lines[4], "✓ done in") {
		t.Errorf("region and footer:\n%s", h.scr)
	}
}

func TestLivePromptHoldsTheScreen(t *testing.T) {
	h := newHarness(t, renderLive, 80, 24, true)
	p := progress.Start(h.ctx, "upgrade", progress.Files, 10)
	p.Add(3)
	h.step(time.Second)
	fmt.Fprint(h.out, "Delete the block files? [y/N] ")
	h.step(100 * time.Millisecond)
	if strings.Contains(h.scr.String(), "[y/N]") {
		t.Fatal("a partial line showed before the hold time")
	}
	h.step(300 * time.Millisecond)
	if !strings.HasSuffix(h.scr.String(), "Delete the block files? [y/N]") || h.scr.Hidden() {
		t.Fatalf("prompt not shown with the cursor:\n%s", h.scr)
	}
	before := h.scr.String()
	p.Add(1)
	h.step(time.Second)
	if h.scr.String() != before {
		t.Fatalf("the display redrew over a prompt:\n%s", h.scr)
	}
	fmt.Fprintln(h.out, "")
	p.Done(nil)
	h.step(time.Second)
	h.rn.stop(nil)
	lines := h.scr.Text()
	if lines[len(lines)-1][:len("✓ done")] != "✓ done" || !strings.Contains(h.scr.String(), "[y/N]") {
		t.Fatalf("after the prompt:\n%s", h.scr)
	}
}

func TestPlainLines(t *testing.T) {
	h := newHarness(t, renderPlain, 80, 24, false)
	progress.Plan(h.ctx, "store", "index", "full-text")
	progress.Skip(h.ctx, "full-text", "no --fts")
	s := progress.Start(h.ctx, "store", progress.Bytes, 4<<30)
	ix := progress.Start(h.ctx, "index", progress.Files, progress.Unknown)
	h.step(time.Second)
	s.Add(1 << 30)
	ix.Add(1200)
	h.step(15 * time.Second)
	fmt.Fprint(h.errw, "partial ")
	s.Add(3 << 30)
	s.Done(nil)
	h.step(time.Second) // the end line waits for the partial line
	if strings.Contains(h.plain.String(), "store: done") {
		t.Fatalf("a line split another writer's partial line:\n%s", h.plain)
	}
	fmt.Fprintln(h.errw, "line")
	ix.Done(errors.New("no partitions"))
	progress.Hint(h.ctx, "run dues reindex X to finish its index")
	h.rn.stop(errors.New("no partitions"))
	got := h.plain.String()
	for _, w := range []string{
		"dues: full-text: skipped: no --fts\n",
		"dues: store: start, 4.00 GiB\n",
		"dues: index: start\n",
		"dues: store: 25%, 1.00 of 4.00 GiB, ",
		"; index 1,200 files\n",
		"partial line\n",
		"dues: store: done, 4.00 GiB in 16.0s, ",
		"dues: index: failed after 1,200 files: no partitions\n",
		"dues: failed after 17.0s\n",
		"dues: hint: run dues reindex X to finish its index\n",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("plain output lacks %q:\n%s", w, got)
		}
	}
	if strings.ContainsAny(got, "\x1b\r{}") {
		t.Errorf("plain output has an escape, a carriage return or a brace:\n%q", got)
	}
	if strings.Contains(got, "dues: index: 1,200") {
		t.Errorf("a second heartbeat line for the step beside the focus:\n%s", got)
	}
}

// Plain heartbeats come every 15 seconds at first, then every tenth of the
// time run, and never more than five minutes apart: a two-hour store prints
// a few dozen, not hundreds.
func TestPlainCadence(t *testing.T) {
	h := newHarness(t, renderPlain, 80, 24, false)
	s := progress.Start(h.ctx, "store", progress.Bytes, 1<<40)
	var beats []time.Duration
	start := h.clock.now()
	for range 2 * 3600 {
		s.Add(1 << 26)
		before := h.plain.Len()
		h.step(time.Second)
		if chunk := h.plain.String()[before:]; strings.Contains(chunk, "dues: store: ") && strings.Contains(chunk, "%, ") {
			beats = append(beats, h.clock.now().Sub(start))
		}
	}
	if len(beats) < 30 || len(beats) > 70 || beats[0] != 15*time.Second {
		t.Fatalf("%d beats, first at %v", len(beats), beats[0])
	}
	for i := 1; i < len(beats); i++ {
		if gap := beats[i] - beats[i-1]; gap < 15*time.Second || gap > 5*time.Minute+time.Second {
			t.Errorf("beat %d comes %v after the one before", i, gap)
		}
	}
	h.rn.stop(nil)
}

// A folder's heartbeat names the item's step and counts the items.
func TestPlainItemsBeat(t *testing.T) {
	h := newHarness(t, renderPlain, 80, 24, false)
	all := progress.Start(h.ctx, "evidence", progress.Evidence, 4)
	item := progress.Start(progress.Within(h.ctx, all), "b.dd", progress.None, 0)
	st := progress.Start(progress.Within(h.ctx, item), "store", progress.Bytes, 100)
	st.Add(50)
	h.step(16 * time.Second)
	if got := h.plain.String(); !strings.Contains(got, "dues: evidence / b.dd / store: 50%, 50 of 100 B, ") ||
		!strings.Contains(got, ", overall 12% (0 of 4 evidence)") {
		t.Fatalf("plain output:\n%s", got)
	}
	h.rn.stop(nil)
}

func TestFolderItemsGoToScrollback(t *testing.T) {
	h := newHarness(t, renderLive, 80, 24, false)
	all := progress.Start(h.ctx, "evidence", progress.Evidence, 2)
	for _, name := range []string{"a.dd", "b.dd"} {
		item := progress.Start(progress.Within(h.ctx, all), name, progress.None, 0)
		st := progress.Start(progress.Within(h.ctx, item), "store", progress.Bytes, 10)
		h.step(time.Second)
		h.screenHas(name)
		st.Add(10)
		st.Done(nil)
		item.Done(nil)
		all.Add(1)
		h.step(time.Second)
	}
	all.Done(nil)
	h.rn.stop(nil)
	lines := h.scr.Text()
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "    ✓ a.dd") || !strings.HasPrefix(lines[1], "    ✓ b.dd") ||
		!strings.HasPrefix(lines[2], "  ✓ evidence") || !strings.HasPrefix(lines[3], "✓ done") {
		t.Fatalf("folder screen:\n%s", h.scr)
	}
}

func TestPanicRestoresTheTerminal(t *testing.T) {
	h := newHarness(t, renderLive, 80, 24, false)
	h.rn.stop(nil) // the harness started one runner; this test makes its own
	scr := newScreen(t, 80)
	restored := false
	cfg := h.rn.cfg
	cfg.progress = scr
	cfg.wrap = func(f func(out, err io.Writer) (io.Writer, io.Writer)) func() {
		f(io.Discard, scr)
		return func() { restored = true }
	}
	defer func() {
		if recover() == nil {
			t.Fatal("the panic did not go on")
		}
		if scr.Hidden() || !restored || !strings.Contains(scr.String(), "failed") {
			t.Fatalf("after a panic: hidden %v restored %v\n%s", scr.Hidden(), restored, scr)
		}
	}()
	_ = run(context.Background(), cfg, func(ctx context.Context) error {
		progress.Start(ctx, "store", progress.Bytes, 10).Add(3)
		panic("boom")
	})
}

func TestEmergency(t *testing.T) {
	h := newHarness(t, renderLive, 80, 24, false)
	p := progress.Start(h.ctx, "store", progress.Bytes, 10)
	p.Add(4)
	h.step(time.Second)
	h.rn.emergency()
	h.screenHas("■ store", "interrupted", "stopped at once")
	if h.scr.Hidden() {
		t.Fatal("cursor hidden after a forced exit")
	}
	h.rn.stop(nil) // a later stop changes nothing
}

func TestRunOffWithoutReporter(t *testing.T) {
	err := run(context.Background(), runConfig{mode: renderOff}, func(ctx context.Context) error {
		if progress.From(ctx) != nil {
			return errors.New("a reporter with output silenced")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRunConcurrent is for -race: workers add while the sampler draws.
func TestRunConcurrent(t *testing.T) {
	scr := newScreen(t, 120)
	cfg := runConfig{
		mode: renderLive, now: time.Now, tick: time.Millisecond, heartbeat: time.Second, hold: time.Second,
		size: func() (int, int) { return 120, 40 }, painter: Painter{glyphs: GlyphsFull, decorated: true},
		progress: scr, wrap: func(f func(out, err io.Writer) (io.Writer, io.Writer)) func() {
			f(io.Discard, scr)
			return func() {}
		},
	}
	err := run(context.Background(), cfg, func(ctx context.Context) error {
		p := progress.Start(ctx, "store", progress.Bytes, 32*20000)
		var wg sync.WaitGroup
		for range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 20000 {
					p.Add(1)
				}
			}()
		}
		wg.Wait()
		p.Done(nil)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(scr.String(), "✓ store") {
		t.Fatalf("screen:\n%s", scr)
	}
}
