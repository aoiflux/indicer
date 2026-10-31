package ui

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"indicer/internal/console"
)

// The extras are rare and stay out of the way. MaybeEgg plays one only
// when Mode.Fun allows it (stdin, stdout and stderr are terminals, not CI,
// not --plain or --no-fun, not a build without fun), after a long command
// that succeeded, by a small chance, once per process. They draw on stderr,
// erase themselves, write nothing to the log and change no exit code.
const (
	eggChance = 0.02
	eggAfter  = 10 * time.Second
)

// Test seams.
var (
	eggRand  = rand.Float64
	eggPick  = rand.IntN
	eggSleep = sleeper(sleepCtx)
	eggDone  atomic.Bool
)

// extra is one of the extras: what it plays, and after which commands.
type extra struct {
	name  string
	after []string                // kingpin FullCommand names; empty: any
	anim  func(Painter) Animation // nil: the sign-off line
}

// extras are chosen by the command that ran, so each fits what was done.
var extras = []extra{
	{name: "cat_in_box", after: []string{"store", "restore"}, anim: catInBox},
	{name: "evidence_ghost", after: []string{"store", "reindex"}, anim: ghost},
	{name: "magnifying_glass", after: []string{"search", "near in", "near out"}, anim: magnifier},
	{name: "artefact_found", after: []string{"micro extract", "search", "enrich"}, anim: artefactFound},
	{name: "sign_off"},
}

// extrasFor are the extras that may follow command.
func extrasFor(command string) []extra {
	var out []extra
	for _, x := range extras {
		if len(x.after) == 0 || slices.Contains(x.after, command) {
			out = append(out, x)
		}
	}
	return out
}

// EggCommands are the commands the extras are chosen by, so a test can
// check each is still a command.
func EggCommands() []string {
	var out []string
	for _, x := range extras {
		for _, c := range x.after {
			if !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
	}
	return out
}

// MaybeEgg perhaps plays one of the extras after command succeeded and ran
// for ran.
func MaybeEgg(command string, ran time.Duration) {
	m := Current()
	if !m.Fun || ran < eggAfter || eggDone.Load() || eggRand() >= eggChance {
		return
	}
	if !eggDone.CompareAndSwap(false, true) {
		return
	}
	w := console.Err()
	p := For(w)
	if width, _ := console.Size(); width < maxCols+2 {
		return
	}
	fit := extrasFor(command)
	x := fit[eggPick(len(fit))]
	if x.anim == nil {
		signOff(p)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	play(ctx, w, x.anim(p), eggSleep)
}

// signOff is the quiet extra: one line from `dues fortune` under the
// footer, which is how anyone finds the command.
func signOff(p Painter) {
	width, _ := console.Size()
	line := "  " + fortuneLine(p, fortunes[eggPick(len(fortunes))]) + " " + p.Paint(Muted, p.Glyphs().Sep+" dues fortune")
	fmt.Fprintln(console.Err(), wrapLine(line, width-1, "    "))
}

// catInBox: a cat peeks out of evidence box #0042, winks, and is gone.
func catInBox(p Painter) Animation {
	box := []string{p.Paint(Evidence, " .-------."), p.Paint(Evidence, " | #0042 |")}
	ears, face, wink := "   /\\_/\\", "  ( o.o )", "  ( -.o )"
	frame := func(top, mid string) Frame {
		return Frame{Rows: []string{top, mid, box[0], box[1]}, Hold: 150 * time.Millisecond}
	}
	closed, peek, look, winked := frame("", ""), frame("", ears), frame(ears, face), frame(ears, wink)
	return Animation{Name: "cat_in_box", Frames: []Frame{closed, peek, look, winked, look, peek, closed, closed}}
}

// catPeek is the cat for a terminal that may not animate.
func catPeek(p Painter) []string {
	return []string{"   /\\_/\\", "  ( o.o )", p.Paint(Evidence, " .-------."), p.Paint(Evidence, " | #0042 |")}
}

// magnifier: a lens scans a PE header's first bytes and finds MZ.
func magnifier(p Painter) Animation {
	bytes := strings.Fields("4D 5A 90 00 03 00 00 00 04 00")
	strip := func(at int) string {
		var b strings.Builder
		for i, x := range bytes {
			switch {
			case i == at:
				b.WriteString(p.Paint(Highlight, "["))
			case i == at+2:
				b.WriteString(p.Paint(Highlight, "]"))
			default:
				b.WriteString(" ")
			}
			if i == at || i == at+1 {
				b.WriteString(p.Strong(Highlight, x))
			} else {
				b.WriteString(p.Paint(Muted, x))
			}
		}
		if at+2 == len(bytes) {
			b.WriteString(p.Paint(Highlight, "]"))
		}
		return b.String()
	}
	var frames []Frame
	for at := len(bytes) - 2; at >= 0; at-- {
		frames = append(frames, Frame{Rows: []string{strip(at)}, Hold: 90 * time.Millisecond})
	}
	found := strip(0) + "  " + p.Strong(Success, p.Glyphs().OK) + " " + p.Bold("MZ")
	frames = append(frames, Frame{Rows: []string{found}, Hold: 180 * time.Millisecond})
	return Animation{Name: "magnifying_glass", Frames: frames}
}

// ghost: a deleted file drifts up from unallocated space, waves, and fades.
func ghost(p Painter) Animation {
	const icon, caption = "~[x]~", "a deleted file waves hello"
	return Animation{Name: "evidence_ghost", Frames: []Frame{
		{Rows: []string{p.Paint(Muted, icon)}, Hold: 200 * time.Millisecond},
		{Rows: []string{icon + "  " + p.Paint(Muted, caption)}, Hold: 200 * time.Millisecond},
		{Rows: []string{icon + "  " + caption}, Hold: 200 * time.Millisecond},
		{Rows: []string{p.Paint(Muted, icon)}, Hold: 200 * time.Millisecond},
	}}
}

// artefactFound: a gem glints where an artefact was found. It says no word
// that could be read as a finding.
func artefactFound(p Painter) Animation {
	dim, gem, glint := "◇", "◆", "·◆·"
	if p.GlyphSet() != GlyphsFull {
		dim, gem, glint = "o", "*", ".*."
	}
	return Animation{Name: "artefact_found", Frames: []Frame{
		{Rows: []string{" " + p.Paint(Muted, dim)}, Hold: 200 * time.Millisecond},
		{Rows: []string{" " + p.Strong(Forensics, gem)}, Hold: 200 * time.Millisecond},
		{Rows: []string{p.Strong(Forensics, glint)}, Hold: 300 * time.Millisecond},
		{Rows: []string{" " + p.Paint(Muted, gem)}, Hold: 200 * time.Millisecond},
	}}
}

// Meow is `dues mew`: the cat in the box, now, whatever the odds. Where
// it may not animate (a pipe, --plain, --no-fun) the cat is printed.
func Meow() {
	if Current().Fun {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		play(ctx, console.Err(), catInBox(For(console.Err())), eggSleep)
		return
	}
	for _, row := range catPeek(For(console.Out())) {
		fmt.Fprintln(console.Out(), row)
	}
}

// Coffee is `dues coffee`.
func Coffee() { fmt.Fprintln(console.Out(), "Forensic caffeine levels nominal.") }

// Fortune is `dues fortune`: one line from the list.
func Fortune() {
	p := For(console.Out())
	fmt.Fprintln(console.Out(), fortuneLine(p, fortunes[eggPick(len(fortunes))]))
}

func fortuneLine(p Painter, f fortune) string {
	s := `"` + f.text + `"`
	if f.source != "" {
		s += " " + p.Paint(Muted, p.Glyphs().Dash+" "+f.source)
	}
	return s
}
