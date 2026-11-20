package ui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"indicer/internal/progress"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// plainView is a live layout with no colour codes, so tests read the text.
func plainView() liveView {
	return liveView{
		p: Painter{glyphs: GlyphsFull, decorated: true}, g: GlyphsFull.Glyphs(),
		rate: func(uint64) float64 { return 148 << 20 }, now: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC),
	}
}

const gib = 1 << 30

func storeEstimate() progress.Estimate {
	return progress.Estimate{
		Mode: progress.ModeWork, Fraction: 0.81, Steps: 2, Step: 2, InWork: true,
		Focus: progress.View{Name: "store", State: progress.Running, Unit: progress.Bytes, Done: 2802466160, Total: 4509715661}, // 2.61 of 4.20 GiB
		Rate:  148 << 20, ETA: 11 * time.Second, HasETA: true, Whole: true, Approx: true,
	}
}

func TestOverallRowModes(t *testing.T) {
	lv := plainView()
	index := progress.View{Name: "index", State: progress.Running, Unit: progress.Files, Done: 121804}
	notWhole := storeEstimate()
	notWhole.Whole = false
	oneStep := storeEstimate()
	oneStep.Steps, oneStep.Step = 1, 1
	waiting := progress.Estimate{Mode: progress.ModeWork, Fraction: 0.5, Steps: 2, Step: 2, Focus: index, Rate: 3120}
	for name, c := range map[string]struct {
		e         progress.Estimate
		want, not []string
	}{
		"work":            {storeEstimate(), []string{"━", "81%", "step 2/2", "2.61/4.20 GiB", "148 MiB/s", "ETA ~11s"}, []string{"storing"}},
		"work, one step":  {oneStep, []string{"81%", "storing", "2.61/4.20 GiB"}, []string{"step"}},
		"work, more left": {notWhole, []string{"81%", "step 2/2"}, []string{"ETA"}},
		"work, waiting":   {waiting, []string{"50%", "step 2/2", "indexing", "121,804 files", "3,120 files/s"}, nil},
		"items": {progress.Estimate{
			Mode: progress.ModeItems, Fraction: 0.31, Done: 12, Total: 340, Unit: progress.Evidence,
			Focus: progress.View{Name: "store", State: progress.Running, Unit: progress.Bytes}, Rate: 148 << 20,
			ETA: 3 * time.Minute, HasETA: true, Whole: true, Approx: true,
		}, []string{"31%", "12/340 evidence", "148 MiB/s", "ETA ~3m00s"}, nil},
		"step": {progress.Estimate{
			Mode: progress.ModeStep, Fraction: 0.267, Whole: true,
			Focus: progress.View{Name: "full-text", State: progress.Running, Unit: progress.Files, Done: 1203, Total: 4500}, Rate: 52,
		}, []string{"26%", "extracting text", "1,203/4,500 files", "52 files/s"}, []string{"ETA"}},
		"activity": {progress.Estimate{Mode: progress.ModeActivity, Focus: index, Rate: 3120, Elapsed: 124 * time.Second},
			[]string{"━", "indexing", "121,804 files", "3,120 files/s", "2m04s"}, []string{"%"}},
		"none": {progress.Estimate{Mode: progress.ModeNone, Elapsed: 12 * time.Second}, []string{"working 12.0s"}, nil},
	} {
		row := lv.overall(c.e, 119)
		for _, s := range c.want {
			if !strings.Contains(row, s) {
				t.Errorf("%s: row %q lacks %q", name, row, s)
			}
		}
		for _, s := range c.not {
			if strings.Contains(row, s) {
				t.Errorf("%s: row %q has %q", name, row, s)
			}
		}
	}
}

func TestOverallRowWidths(t *testing.T) {
	lv := plainView()
	for _, c := range []struct {
		width     int
		want, not []string
	}{
		{119, []string{"━", "81%", "step 2/2", "2.61/4.20 GiB", "148 MiB/s", "ETA ~11s"}, nil},
		{79, []string{"━", "81%", "step 2/2", "ETA ~11s"}, nil},
		{59, []string{"━", "81%", "2.61/4.20 GiB", "ETA ~11s"}, []string{"step"}},
		{39, []string{"━", "81%", "ETA ~11s"}, []string{"2.61/4.20"}},
		{29, []string{"81%", "ETA ~11s"}, []string{"MiB/s"}},
	} {
		row := lv.overall(storeEstimate(), c.width)
		if w := ansi.StringWidth(row); w > c.width {
			t.Errorf("width %d: row is %d wide: %q", c.width, w, row)
		}
		for _, s := range c.want {
			if !strings.Contains(row, s) {
				t.Errorf("width %d: row %q lacks %q", c.width, row, s)
			}
		}
		for _, s := range c.not {
			if strings.Contains(row, s) {
				t.Errorf("width %d: row %q has %q", c.width, row, s)
			}
		}
	}
}

// A running step's row gives way from the right: its rate, then the file,
// then the note; the file is cut from the left so its name stays.
func TestDetailRowWidths(t *testing.T) {
	lv := plainView()
	v := progress.View{
		ID: 2, Name: "store", State: progress.Running, Unit: progress.Bytes, Done: 2 * gib, Total: 4 * gib,
		Note: "encrypted", Item: `Windows\System32\config\SOFTWARE`,
		Meters: []progress.MeterView{{Name: "new", Unit: progress.Bytes, Value: gib}},
	}
	for _, c := range []struct {
		width     int
		want, not []string
	}{
		{119, []string{"  ⠋ storing", "50%", "new 1.00 GiB", "50% dedup", "148 MiB/s", "encrypted", `Windows\System32\config\SOFTWARE`}, nil},
		{80, []string{"148 MiB/s", "encrypted", `…\SOFTWARE`}, []string{"Windows"}},
		{60, []string{"50% dedup", "encrypted"}, []string{"MiB/s", "SOFTWARE"}},
		{30, []string{"storing", "50%"}, nil},
	} {
		row := lv.row(v, 0, 0, false, progress.Estimate{}, true, c.width)
		if w := ansi.StringWidth(row); w > c.width {
			t.Errorf("width %d: row is %d wide: %q", c.width, w, row)
		}
		for _, s := range c.want {
			if !strings.Contains(row, s) {
				t.Errorf("width %d: row %q lacks %q", c.width, row, s)
			}
		}
		for _, s := range c.not {
			if strings.Contains(row, s) {
				t.Errorf("width %d: row %q has %q", c.width, row, s)
			}
		}
	}
	// The focus step's own time left, when the overall row has no ETA.
	e := progress.Estimate{HasETA: true, ETA: 11 * time.Second}
	if row := lv.row(v, 0, 0, true, e, true, 119); !strings.Contains(row, "11s left") || strings.Contains(row, "MiB/s") {
		t.Errorf("focus row %q", row)
	}
}

// A whole store fits in seven lines: the overall row, a row a step, and the
// steps still to come on one line.
func TestLiveLayout(t *testing.T) {
	h := newHarness(t, renderLive, 100, 24, false)
	progress.PlanSteps(h.ctx,
		progress.Step{Name: "identify", Unit: progress.Bytes, Total: 4 * gib},
		progress.Step{Name: "store", Unit: progress.Bytes, Total: 4 * gib},
		progress.Step{Name: "index", Unit: progress.Files, Total: progress.Unknown},
		progress.Step{Name: "full-text", Unit: progress.Files, Total: progress.Unknown},
		progress.Step{Name: "entropy", Unit: progress.Bytes, Total: 4 * gib},
		progress.Step{Name: "graph", Unit: progress.Files, Total: progress.Unknown})
	id := progress.Start(h.ctx, "identify", progress.Bytes, 4*gib)
	id.Add(4 * gib)
	id.Done(nil)
	st := progress.Start(h.ctx, "store", progress.Bytes, 4*gib)
	st.Meter("new", progress.Bytes).Add(gib)
	st.Add(2 * gib)
	ix := progress.Start(h.ctx, "index", progress.Files, progress.Unknown)
	ix.Add(1000)
	ix.Note("partition 2, ntfs")
	ix.Item(`Windows\System32\config\SOFTWARE`)
	h.step(3 * time.Second)
	lines := h.scr.Text()
	if len(lines) > liveLines || !strings.Contains(lines[0], "50%") || !strings.Contains(lines[0], "step 2/3") {
		t.Fatalf("live screen:\n%s", h.scr)
	}
	h.screenHas("  ✓ identify", "storing", "new 1.00 GiB", " left", "indexing", "1,000 files", "partition 2, ntfs",
		`config\SOFTWARE`, "· next: full-text · entropy · graph")
	if strings.Contains(lines[0], "ETA") {
		t.Errorf("an overall ETA while entropy waits: %q", lines[0])
	}
	h.rn.stop(errors.New("stop"))
}

// A folder: the running item, and its steps one level in.
func TestLiveItems(t *testing.T) {
	h := newHarness(t, renderLive, 100, 24, false)
	progress.Start(h.ctx, "scan folder", progress.Files, progress.Unknown).Done(nil)
	all := progress.Start(h.ctx, "evidence", progress.Evidence, 3)
	item := progress.Start(progress.Within(h.ctx, all), "case/b.dd", progress.None, 0)
	ictx := progress.Within(h.ctx, item)
	progress.PlanSteps(ictx, progress.Step{Name: "store", Unit: progress.Bytes, Total: 100}, progress.Step{Name: "index", Unit: progress.Files, Total: progress.Unknown})
	progress.Start(ictx, "store", progress.Bytes, 100).Add(50)
	h.step(time.Second)
	lines := h.scr.Text()
	if !strings.Contains(lines[0], "16%") || !strings.Contains(lines[0], "0/3 evidence") {
		t.Fatalf("overall row %q", lines[0])
	}
	h.screenHas("  ✓ scan folder", "case/b.dd  50%", "    ⠋ storing", "    · next: index")
	h.rn.stop(errors.New("stop"))
}

// The final frame keeps the top-level steps, and those that never ran on
// one line.
func TestFinalFrameNotRun(t *testing.T) {
	h := newHarness(t, renderLive, 100, 24, false)
	progress.PlanSteps(h.ctx, progress.Step{Name: "store", Unit: progress.Bytes, Total: 10},
		progress.Step{Name: "index", Unit: progress.Files, Total: progress.Unknown},
		progress.Step{Name: "full-text", Unit: progress.Files, Total: progress.Unknown})
	st := progress.Start(h.ctx, "store", progress.Bytes, 10)
	st.Add(10)
	h.step(time.Second)
	st.Done(nil)
	h.rn.stop(nil)
	lines := h.scr.Text()
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "  ✓ store") || lines[1] != "  – not run: index, full-text" ||
		!strings.HasPrefix(lines[2], "✓ done") {
		t.Fatalf("final screen:\n%s", h.scr)
	}
}

// On a terminal stdout shares, a stored item's report says it was stored,
// so only items that did not end OK print a line of their own.
func TestSharedTerminalItems(t *testing.T) {
	h := newHarness(t, renderLive, 80, 24, true)
	all := progress.Start(h.ctx, "evidence", progress.Evidence, 2)
	for name, err := range map[string]error{"a.dd": nil, "b.dd": errors.New("unreadable")} {
		item := progress.Start(progress.Within(h.ctx, all), name, progress.None, 0)
		item.Done(err)
		all.Add(1)
		h.step(time.Second)
	}
	all.Done(nil)
	h.rn.stop(nil)
	if s := h.scr.String(); strings.Contains(s, "✓ a.dd") || !strings.Contains(s, "    ✗ b.dd") {
		t.Fatalf("screen:\n%s", s)
	}
}

// Between two steps, with nothing running, the last overall row stays up
// to a second.
func TestOverallHeldBetweenSteps(t *testing.T) {
	h := newHarness(t, renderLive, 100, 24, false)
	up := progress.Start(h.ctx, "upgrade", progress.Files, 10)
	up.Add(5)
	h.step(time.Second)
	up.Add(5)
	up.Done(nil)
	h.step(500 * time.Millisecond)
	if lines := h.scr.Text(); !strings.Contains(lines[0], "upgrading") {
		t.Fatalf("held row %q", lines[0])
	}
	h.step(time.Second)
	if lines := h.scr.Text(); !strings.Contains(lines[0], "working") {
		t.Fatalf("after a second %q", lines[0])
	}
	h.rn.stop(nil)
}

// Live detail rows are faint; warnings, failures and the final frame are not.
func TestFaintDetails(t *testing.T) {
	lv := plainView()
	lv.p = Painter{profile: colorprofile.TrueColor, theme: ThemeUnknown, glyphs: GlyphsFull, decorated: true}
	now := lv.now
	running := progress.View{ID: 1, Name: "store", State: progress.Running, Unit: progress.Bytes, Done: 5, Total: 10}
	done := progress.View{ID: 2, Name: "store", State: progress.OK, Unit: progress.Bytes, Done: 10, Total: 10, Started: now.Add(-time.Second), Ended: now}
	failed := progress.View{ID: 3, Name: "store", State: progress.Failed, Unit: progress.Bytes, Done: 5, Total: 10, Reason: "disk full"}
	if row := lv.row(running, 0, 0, false, progress.Estimate{}, true, 100); !strings.Contains(row, "\x1b[2m") {
		t.Errorf("live row not faint: %q", row)
	}
	for name, row := range map[string]string{
		"final":  lv.row(done, 0, 0, false, progress.Estimate{}, false, 100),
		"failed": lv.row(failed, 0, 0, false, progress.Estimate{}, true, 100),
	} {
		if strings.Contains(row, "\x1b[2m") {
			t.Errorf("%s row is faint: %q", name, row)
		}
	}
}

func TestFaint(t *testing.T) {
	for _, c := range []struct {
		p    Painter
		want string
	}{
		{Painter{}, "x"},
		{Painter{profile: colorprofile.Ascii}, "\x1b[2mx\x1b[0m"},
		{Painter{profile: colorprofile.TrueColor}, "\x1b[2mx\x1b[0m"},
		{Painter{profile: colorprofile.Ascii, theme: ThemeDark}, "\x1b[2mx\x1b[0m"},
		{Painter{profile: colorprofile.TrueColor, theme: ThemeDark}, "\x1b[" + sgr(colorprofile.TrueColor, ThemeDark, Muted) + "mx\x1b[0m"},
	} {
		if got := c.p.Faint("x"); got != c.want {
			t.Errorf("%+v: Faint = %q, want %q", c.p, got, c.want)
		}
	}
}

// A step that just ended OK shows its mark in highlight for three frames;
// the scrollback never keeps the pulse.
func TestPulse(t *testing.T) {
	lv := plainView()
	lv.p = Painter{profile: colorprofile.TrueColor, theme: ThemeDark, glyphs: GlyphsFull, decorated: true}
	lv.pulse = map[uint64]int{7: 5}
	v := progress.View{ID: 7, Name: "store", State: progress.OK}
	lv.frame = 4
	if got := lv.mark(v); got != lv.p.Strong(Highlight, lv.g.OK) {
		t.Errorf("frame 4: mark %q", got)
	}
	lv.frame = 5
	if got := lv.mark(v); got != lv.p.Strong(Success, lv.g.OK) {
		t.Errorf("frame 5: mark %q", got)
	}
	lv.frame = 4
	if lines := lv.final(progress.Snapshot{Phases: []progress.View{v}}, 80); strings.Contains(lines[0], lv.p.Strong(Highlight, lv.g.OK)) {
		t.Errorf("the final frame pulses: %q", lines[0])
	}
}

func TestSweep(t *testing.T) {
	g := GlyphsASCII.Glyphs()
	for frame, want := range map[int]string{
		0:  "###-------",
		1:  "-###------",
		7:  "-------###",
		8:  "------###-",
		14: "###-------",
	} {
		if got := sweep(Painter{}, g, 10, frame); got != want {
			t.Errorf("frame %d: %q, want %q", frame, got, want)
		}
	}
}
