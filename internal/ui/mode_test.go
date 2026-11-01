package ui

import (
	"slices"
	"testing"

	"indicer/internal/console"

	"github.com/charmbracelet/colorprofile"
)

func TestPreScan(t *testing.T) {
	cases := []struct {
		args []string
		want Options
		rest []string
	}{
		{[]string{"store", "x"}, Options{}, []string{"store", "x"}},
		{[]string{"--plain", "list"}, Options{Plain: true}, []string{"list"}},
		{[]string{"list", "--color=never", "--no-fun"}, Options{Color: ColorNever, NoFun: true}, []string{"list"}},
		{[]string{"--color", "always", "-h"}, Options{Color: ColorAlways}, []string{"-h"}},
		{[]string{"--color=ANSI"}, Options{Color: ColorANSI}, []string{}},
		{[]string{"--color", "truecolor"}, Options{Color: ColorTrueColor}, []string{}},
		{[]string{"--color", "bogus", "list"}, Options{}, []string{"list"}},
		{[]string{"--ci", "--no-ci", "--plain", "--no-plain"}, Options{}, []string{}},
		{[]string{"store", "--", "--plain"}, Options{}, []string{"store", "--", "--plain"}},
		{[]string{"--color"}, Options{}, []string{}},
		{[]string{"-d", "./case", "store", "--ci", "-h"}, Options{CI: true}, []string{"-d", "./case", "store", "-h"}},
		{[]string{"--theme", "light", "tui"}, Options{Theme: ThemeLight}, []string{"tui"}},
		{[]string{"--theme=DARK"}, Options{Theme: ThemeDark}, []string{}},
		{[]string{"--theme=auto"}, Options{}, []string{}},
		{[]string{"--theme"}, Options{}, []string{}},
		{[]string{"--taskbar", "store"}, Options{Taskbar: true}, []string{"store"}},
		{[]string{"--taskbar", "--no-taskbar"}, Options{}, []string{}},
	}
	for _, c := range cases {
		got, rest := PreScan(c.args)
		if got != c.want || !slices.Equal(rest, c.rest) {
			t.Errorf("PreScan(%q) = %+v, %q; want %+v, %q", c.args, got, rest, c.want, c.rest)
		}
	}
}

// allTTY is an interactive session: every stream a terminal.
var allTTY = console.Terminal{InTTY: true, OutTTY: true, ErrTTY: true, Shared: true}

func TestResolve(t *testing.T) {
	pipeOut := allTTY
	pipeOut.OutTTY, pipeOut.Shared = false, false
	noStdin := allTTY
	noStdin.InTTY = false

	type want struct {
		out, err    colorprofile.Profile
		glyphs      GlyphSet
		motion, fun bool
		interactive bool
	}
	full := want{colorprofile.ANSI256, colorprofile.ANSI256, GlyphsFull, true, true, true}
	cases := []struct {
		name string
		o    Options
		t    console.Terminal
		want want
	}{
		{"interactive", Options{}, allTTY, full},
		{"--color=auto", Options{Color: ColorAuto}, allTTY, full},
		{"--color=truecolor", Options{Color: ColorTrueColor}, allTTY,
			want{colorprofile.TrueColor, colorprofile.TrueColor, GlyphsFull, true, true, true}},
		{"--color=ansi", Options{Color: ColorANSI}, allTTY,
			want{colorprofile.ANSI, colorprofile.ANSI, GlyphsFull, true, true, true}},
		{"--color=never", Options{Color: ColorNever}, allTTY,
			want{colorprofile.NoTTY, colorprofile.NoTTY, GlyphsFull, true, true, true}},
		{"stdout piped", Options{}, pipeOut,
			want{colorprofile.NoTTY, colorprofile.ANSI256, GlyphsFull, true, false, true}},
		{"--color=truecolor on a pipe", Options{Color: ColorTrueColor}, pipeOut,
			want{colorprofile.NoTTY, colorprofile.TrueColor, GlyphsFull, true, false, true}},
		{"--color=always on a pipe", Options{Color: ColorAlways}, pipeOut,
			want{colorprofile.ANSI256, colorprofile.ANSI256, GlyphsFull, true, false, true}},
		{"--ci", Options{CI: true}, allTTY,
			want{colorprofile.NoTTY, colorprofile.NoTTY, GlyphsFull, false, false, false}},
		{"--ci with --color=always", Options{CI: true, Color: ColorAlways}, allTTY,
			want{colorprofile.ANSI256, colorprofile.ANSI256, GlyphsFull, false, false, false}},
		{"--plain", Options{Plain: true}, allTTY,
			want{colorprofile.NoTTY, colorprofile.NoTTY, GlyphsASCII, false, false, true}},
		{"--plain with --color=always", Options{Plain: true, Color: ColorAlways}, allTTY,
			want{colorprofile.ANSI256, colorprofile.ANSI256, GlyphsASCII, false, false, true}},
		{"--plain with --color=truecolor", Options{Plain: true, Color: ColorTrueColor}, allTTY,
			want{colorprofile.NoTTY, colorprofile.NoTTY, GlyphsASCII, false, false, true}},
		{"--no-fun", Options{NoFun: true}, allTTY,
			want{colorprofile.ANSI256, colorprofile.ANSI256, GlyphsFull, true, false, true}},
		{"stdin not a terminal", Options{}, noStdin,
			want{colorprofile.ANSI256, colorprofile.ANSI256, GlyphsFull, true, false, false}},
		{"nothing a terminal", Options{}, console.Terminal{},
			want{colorprofile.NoTTY, colorprofile.NoTTY, GlyphsFull, false, false, false}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := Resolve(c.o, c.t)
			got := want{m.Out.Profile, m.Err.Profile, m.Out.Glyphs, m.Err.Motion, m.Fun, m.Interactive}
			if got != c.want {
				t.Fatalf("got %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func TestFunDefaultOff(t *testing.T) {
	prev := funDefault
	funDefault = "off"
	t.Cleanup(func() { funDefault = prev })
	if Resolve(Options{}, allTTY).Fun {
		t.Fatal("fun on in a build with funDefault=off")
	}
}

func TestTheme(t *testing.T) {
	for _, th := range []Theme{ThemeUnknown, ThemeDark, ThemeLight} {
		if got := Resolve(Options{Theme: th}, allTTY).Theme; got != th {
			t.Errorf("--theme %d: theme %d", th, got)
		}
	}
	for i, name := range ThemeNames {
		if got := parseTheme(name); got != Theme(i) {
			t.Errorf("parseTheme(%q) = %d, want %d", name, got, i)
		}
	}
}

// Taskbar progress needs --taskbar and a stderr that may move: never in CI,
// on a piped stderr or under --plain.
func TestTaskbarGate(t *testing.T) {
	stderrPiped := console.Terminal{InTTY: true, OutTTY: true}
	for _, c := range []struct {
		o    Options
		tty  console.Terminal
		want bool
	}{
		{Options{Taskbar: true}, allTTY, true},
		{Options{}, allTTY, false},
		{Options{Taskbar: true, CI: true}, allTTY, false},
		{Options{Taskbar: true, Plain: true}, allTTY, false},
		{Options{Taskbar: true}, stderrPiped, false},
	} {
		if m := Resolve(c.o, c.tty); m.Taskbar != c.want {
			t.Errorf("%+v, %+v: Taskbar %v, want %v", c.o, c.tty, m.Taskbar, c.want)
		}
	}
}
