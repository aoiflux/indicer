package ui

import (
	"context"
	"fmt"
	"io"
	"slices"
	"testing"
	"time"

	"indicer/internal/progress"
)

func TestOSC94(t *testing.T) {
	if got := osc94(tbNormal, 45); got != "\x1b]9;4;1;45\x1b\\" {
		t.Errorf("osc94 = %q", got)
	}
	if got := osc94(tbClear, 0); got != "\x1b]9;4;0;0\x1b\\" {
		t.Errorf("clear = %q", got)
	}
}

// The tab gets a new state at once and a new percentage at most twice a
// second; nothing is sent twice, and only what was shown is cleared.
func TestTaskbarNext(t *testing.T) {
	var tb taskbar
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	if s := tb.clear(); s != "" {
		t.Errorf("clear before anything was sent: %q", s)
	}
	steps := []struct {
		st   tbState
		pct  int
		at   time.Duration
		want string
	}{
		{tbNormal, 10, 0, osc94(tbNormal, 10)},
		{tbNormal, 10, time.Second, ""},
		{tbNormal, 20, 1100 * time.Millisecond, osc94(tbNormal, 20)},
		{tbNormal, 30, 1300 * time.Millisecond, ""},
		{tbPaused, 30, 1400 * time.Millisecond, osc94(tbPaused, 30)},
		{tbNormal, 150, 2 * time.Second, osc94(tbNormal, 100)},
		{tbIndeterminate, 40, 2100 * time.Millisecond, osc94(tbIndeterminate, 0)},
		{tbNormal, -5, 2200 * time.Millisecond, osc94(tbNormal, 0)},
	}
	for i, s := range steps {
		if got := tb.next(s.st, s.pct, t0.Add(s.at)); got != s.want {
			t.Errorf("step %d: %q, want %q", i, got, s.want)
		}
	}
	if !taskbarShown.Load() {
		t.Error("taskbarShown not set")
	}
	if s := tb.clear(); s != osc94(tbClear, 0) || taskbarShown.Load() {
		t.Errorf("clear: %q, shown %v", s, taskbarShown.Load())
	}
	if s := tb.clear(); s != "" {
		t.Errorf("a second clear: %q", s)
	}
}

// Progress on the tab: nothing for the first second, then the percentage
// at most twice a second, paused while a prompt waits, cleared at the end.
func TestTaskbarProgress(t *testing.T) {
	h := newTaskbarHarness(t)
	p := progress.Start(h.ctx, "upgrade", progress.Files, 100)
	p.Add(20)
	h.step(500 * time.Millisecond)
	if got := h.scr.Taskbar(); len(got) != 0 {
		t.Fatalf("before a second: %q", got)
	}
	h.step(600 * time.Millisecond)
	p.Add(20)
	h.step(100 * time.Millisecond)
	h.step(500 * time.Millisecond)
	fmt.Fprint(h.out, "Continue? [y/N] ")
	h.step(100 * time.Millisecond)
	h.step(300 * time.Millisecond)
	fmt.Fprintln(h.out, "")
	p.Done(nil)
	h.step(time.Second)
	h.rn.stop(nil)
	h.scr.Finish()
	got := h.scr.Taskbar()
	if len(got) < 4 || !slices.Equal(got[:3], []string{"1;20", "1;40", "4;40"}) || got[len(got)-1] != "0;0" {
		t.Fatalf("taskbar states %q", got)
	}
}

// A second Ctrl-C takes the progress off the tab, whether or not the
// display's lock is free.
func TestTaskbarEmergency(t *testing.T) {
	for _, locked := range []bool{false, true} {
		h := newTaskbarHarness(t)
		p := progress.Start(h.ctx, "store", progress.Bytes, 10)
		p.Add(4)
		h.step(1100 * time.Millisecond)
		if locked {
			h.rn.mu.Lock()
			h.rn.emergency()
			h.rn.mu.Unlock()
		} else {
			h.rn.emergency()
		}
		if got := h.scr.Taskbar(); len(got) < 2 || got[0] != "1;40" || got[len(got)-1] != "0;0" || h.scr.Hidden() {
			t.Errorf("locked %v: taskbar %q, cursor hidden %v", locked, got, h.scr.Hidden())
		}
		h.rn.stop(nil)
	}
}

// A panic in the command still clears the tab.
func TestTaskbarPanic(t *testing.T) {
	h := newTaskbarHarness(t)
	h.rn.stop(nil) // this test makes its own runner
	scr := newScreenTaskbar(t, 100)
	cfg := h.rn.cfg
	cfg.progress, cfg.manual, cfg.tick, cfg.taskbarDelay, cfg.now = scr, false, time.Millisecond, 0, time.Now
	cfg.wrap = func(f func(out, err io.Writer) (io.Writer, io.Writer)) func() {
		f(io.Discard, scr)
		return func() {}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("the panic did not go on")
		}
		if got := scr.Taskbar(); len(got) == 0 || got[len(got)-1] != "0;0" {
			t.Fatalf("taskbar after a panic: %q", got)
		}
	}()
	_ = run(context.Background(), cfg, func(ctx context.Context) error {
		progress.Start(ctx, "store", progress.Bytes, 10).Add(3)
		for deadline := time.Now().Add(5 * time.Second); len(scr.Taskbar()) == 0 && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		panic("boom")
	})
}
