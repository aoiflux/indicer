package ui

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"indicer/internal/console"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

func instant(context.Context, time.Duration) bool { return true }

func catalogue(p Painter) []Animation {
	return []Animation{catInBox(p), magnifier(p), ghost(p), artefactFound(p)}
}

// TestCatalogueLimits: every animation is at most 1.5 s, holds each frame
// at least 80 ms, and fits 4 rows by 40 columns, in colour and without.
func TestCatalogueLimits(t *testing.T) {
	for _, p := range []Painter{{}, {profile: colorprofile.TrueColor, glyphs: GlyphsFull, decorated: true}} {
		for _, a := range catalogue(p) {
			if d := a.duration(); d > maxAnimation || d <= 0 {
				t.Errorf("%s lasts %v", a.Name, d)
			}
			if a.rows() > maxRows {
				t.Errorf("%s has %d rows", a.Name, a.rows())
			}
			for i, f := range a.Frames {
				if f.Hold < minFrame {
					t.Errorf("%s frame %d holds %v", a.Name, i, f.Hold)
				}
				if len(f.Rows) > maxRows {
					t.Errorf("%s frame %d has %d rows", a.Name, i, len(f.Rows))
				}
				for _, r := range f.Rows {
					if w := ansi.StringWidth(r); w > maxCols {
						t.Errorf("%s frame %d row is %d wide: %q", a.Name, i, w, r)
					}
				}
			}
		}
	}
}

// TestPlayLeavesNothing: an animation erases itself and shows the cursor,
// leaving what was above it, with only the allowed escape sequences (the
// screen fails the test on any other).
func TestPlayLeavesNothing(t *testing.T) {
	for _, a := range catalogue(Painter{profile: colorprofile.ANSI256, glyphs: GlyphsFull, decorated: true}) {
		scr := newScreen(t, 80)
		scr.Write([]byte("✓ done in 38.2s\n"))
		var shown []string
		play(context.Background(), scr, a, func(context.Context, time.Duration) bool {
			shown = append(shown, scr.String())
			return true
		})
		if got := scr.String(); got != "✓ done in 38.2s" || scr.Hidden() {
			t.Errorf("%s left:\n%q (cursor hidden %v)", a.Name, got, scr.Hidden())
		}
		if len(shown) != len(a.Frames) || !strings.Contains(strings.Join(shown, "\n"), "✓ done in 38.2s") {
			t.Errorf("%s drew over what was above it", a.Name)
		}
	}
}

func TestPlayStopsOnCancel(t *testing.T) {
	scr := newScreen(t, 80)
	frames := 0
	play(context.Background(), scr, catInBox(Painter{}), func(context.Context, time.Duration) bool {
		frames++
		return false // Ctrl-C during the first frame
	})
	if frames != 1 || scr.String() != "" || scr.Hidden() {
		t.Fatalf("after a cancel: %d frames, screen %q, cursor hidden %v", frames, scr.String(), scr.Hidden())
	}
}

func TestMaybeEgg(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	fun := ttyMode(colorprofile.TrueColor, ThemeDark, GlyphsFull)
	fun.Fun = true
	noFun := fun
	noFun.Fun = false
	prevRand, prevPick, prevSleep := eggRand, eggPick, eggSleep
	t.Cleanup(func() { eggRand, eggPick, eggSleep = prevRand, prevPick, prevSleep; eggDone.Store(false) })
	eggSleep = instant
	eggPick = func(int) int { return 0 }

	cases := []struct {
		name  string
		mode  Mode
		ran   time.Duration
		rolls []float64
		plays bool
	}{
		{"no fun", noFun, time.Minute, []float64{0, 0}, false},
		{"too quick", fun, 9 * time.Second, []float64{0, 0}, false},
		{"unlucky", fun, time.Minute, []float64{0.021, 0}, false},
		{"lucky", fun, time.Minute, []float64{0.019, 0}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eggDone.Store(false)
			rolls := c.rolls
			eggRand = func() float64 { r := rolls[0]; rolls = rolls[1:]; return r }
			_, errb := terminal(t, c.mode)
			MaybeEgg("store", c.ran)
			if played := errb.Len() > 0; played != c.plays {
				t.Fatalf("played %v, want %v: %q", played, c.plays, errb.String())
			}
			if c.plays {
				// Once per process.
				eggRand = func() float64 { return 0 }
				errb.Reset()
				MaybeEgg("store", c.ran)
				if errb.Len() > 0 {
					t.Fatal("a second extra in one process")
				}
			}
		})
	}
}

// Each command gets the extras that fit what it did; repair, whose output
// scripts read, gets none from main, and only the sign-off here.
func TestExtrasFor(t *testing.T) {
	for cmd, want := range map[string][]string{
		"store":         {"cat_in_box", "evidence_ghost", "sign_off"},
		"restore":       {"cat_in_box", "sign_off"},
		"reindex":       {"evidence_ghost", "sign_off"},
		"search":        {"magnifying_glass", "artefact_found", "sign_off"},
		"near in":       {"magnifying_glass", "sign_off"},
		"near out":      {"magnifying_glass", "sign_off"},
		"micro extract": {"artefact_found", "sign_off"},
		"enrich":        {"artefact_found", "sign_off"},
		"repair":        {"sign_off"},
	} {
		var got []string
		for _, x := range extrasFor(cmd) {
			got = append(got, x.name)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: %q, want %q", cmd, got, want)
		}
	}
}

// The extra played is the one picked among those that fit the command.
func TestMaybeEggByCommand(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	fun := ttyMode(colorprofile.TrueColor, ThemeDark, GlyphsFull)
	fun.Fun = true
	prevRand, prevPick, prevSleep := eggRand, eggPick, eggSleep
	t.Cleanup(func() { eggRand, eggPick, eggSleep = prevRand, prevPick, prevSleep; eggDone.Store(false) })
	eggRand, eggSleep = func() float64 { return 0 }, instant
	for _, c := range []struct {
		cmd    string
		pick   int
		marker string
	}{
		{"store", 0, "#0042"},
		{"store", 1, "~[x]~"},
		{"reindex", 0, "~[x]~"},
		{"search", 0, "MZ"},
		{"search", 1, "◆"},
		{"micro extract", 0, "◆"},
		{"enrich", 0, "◆"},
		{"restore", 1, "dues fortune"},
	} {
		eggDone.Store(false)
		eggPick = func(int) int { return c.pick }
		_, errb := terminal(t, fun)
		MaybeEgg(c.cmd, time.Minute)
		if !strings.Contains(ansi.Strip(errb.String()), c.marker) {
			t.Errorf("%s, pick %d: %q lacks %q", c.cmd, c.pick, errb.String(), c.marker)
		}
	}
}

// The chance stays rare, and nothing plays where no person watches.
func TestEggChance(t *testing.T) {
	if eggChance < 0.01 || eggChance > 0.03 {
		t.Errorf("eggChance %v", eggChance)
	}
	stdoutPiped := console.Terminal{InTTY: true, ErrTTY: true}
	for name, m := range map[string]Mode{
		"CI":           Resolve(Options{}, []string{"CI=true"}, allTTY, "linux"),
		"stdout piped": Resolve(Options{}, nil, stdoutPiped, "linux"),
		"--no-fun":     Resolve(Options{NoFun: true}, nil, allTTY, "linux"),
	} {
		if m.Fun {
			t.Errorf("%s: extras allowed", name)
		}
	}
}

func TestHiddenCommands(t *testing.T) {
	out, _ := terminal(t, Mode{})
	Coffee()
	if out.String() != "Forensic caffeine levels nominal.\n" {
		t.Errorf("coffee %q", out.String())
	}
	out.Reset()
	prevPick := eggPick
	t.Cleanup(func() { eggPick = prevPick })
	eggPick = func(int) int { return 0 }
	Fortune()
	if line, want := out.String(), fortuneLine(Painter{}, fortunes[0])+"\n"; line != want {
		t.Errorf("fortune %q, want %q", line, want)
	}
	out.Reset()
	Meow() // no terminal: the cat is printed, not animated
	if got := out.String(); !strings.HasSuffix(got, " | #0042 |\n") || strings.Count(got, "\n") != 4 {
		t.Errorf("mew on a pipe %q", got)
	}
}

// TestFortuneTone keeps the list to the craft.
func TestFortuneTone(t *testing.T) {
	banned := []string{"victim", "suspect", "crime", "criminal", "murder", "court", "judge", "jury", "police", "agency", "lawyer", "attorney", "defendant", "prosecut"}
	for _, f := range fortunes {
		low := strings.ToLower(f.text)
		for _, b := range banned {
			if strings.Contains(low, b) {
				t.Errorf("fortune %q mentions %q", f.text, b)
			}
		}
		if f.source != "" && !strings.HasPrefix(f.source, "Sherlock Holmes, ") {
			t.Errorf("fortune %q: quoted sources are public-domain Holmes stories only", f.text)
		}
	}
}
