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
		{[]string{"--color", "bogus", "list"}, Options{}, []string{"list"}},
		{[]string{"--ci", "--no-ci", "--plain", "--no-plain"}, Options{}, []string{}},
		{[]string{"store", "--", "--plain"}, Options{}, []string{"store", "--", "--plain"}},
		{[]string{"--color"}, Options{}, []string{}},
		{[]string{"-d", "./case", "store", "--ci", "-h"}, Options{CI: true}, []string{"-d", "./case", "store", "-h"}},
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

// linux is a typical Linux terminal environment.
var linux = []string{"TERM=xterm-256color", "LANG=en_US.UTF-8"}

func env(extra ...string) []string { return append(slices.Clone(linux), extra...) }

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
		env  []string
		t    console.Terminal
		goos string
		want want
	}{
		{"interactive", Options{}, env(), allTTY, "linux", full},
		{"truecolor", Options{}, env("COLORTERM=truecolor"), allTTY, "linux",
			want{colorprofile.TrueColor, colorprofile.TrueColor, GlyphsFull, true, true, true}},
		{"NO_COLOR=1", Options{}, env("NO_COLOR=1"), allTTY, "linux",
			want{colorprofile.Ascii, colorprofile.Ascii, GlyphsFull, true, true, true}},
		{"NO_COLOR=yes", Options{}, env("NO_COLOR=yes"), allTTY, "linux",
			want{colorprofile.Ascii, colorprofile.Ascii, GlyphsFull, true, true, true}},
		{"flag beats NO_COLOR", Options{Color: ColorAlways}, env("NO_COLOR=1"), allTTY, "linux", full},
		{"--color=never", Options{Color: ColorNever}, env(), allTTY, "linux",
			want{colorprofile.NoTTY, colorprofile.NoTTY, GlyphsFull, true, true, true}},
		{"DUES_COLOR=never", Options{}, env("DUES_COLOR=never"), allTTY, "linux",
			want{colorprofile.NoTTY, colorprofile.NoTTY, GlyphsFull, true, true, true}},
		{"flag beats DUES_COLOR", Options{Color: ColorAuto}, env("DUES_COLOR=never"), allTTY, "linux", full},
		{"DUES_COLOR=ansi", Options{}, env("COLORTERM=truecolor", "DUES_COLOR=ansi"), allTTY, "linux",
			want{colorprofile.ANSI, colorprofile.ANSI, GlyphsFull, true, true, true}},
		{"stdout piped", Options{}, env(), pipeOut, "linux",
			want{colorprofile.NoTTY, colorprofile.ANSI256, GlyphsFull, true, false, true}},
		{"FORCE_COLOR on a pipe", Options{}, env("FORCE_COLOR=1"), pipeOut, "linux",
			want{colorprofile.ANSI256, colorprofile.ANSI256, GlyphsFull, true, false, true}},
		{"CLICOLOR_FORCE on a pipe", Options{}, env("CLICOLOR_FORCE=1"), pipeOut, "linux",
			want{colorprofile.ANSI256, colorprofile.ANSI256, GlyphsFull, true, false, true}},
		{"FORCE_COLOR=0 on a pipe", Options{}, env("FORCE_COLOR=0"), pipeOut, "linux",
			want{colorprofile.NoTTY, colorprofile.ANSI256, GlyphsFull, true, false, true}},
		{"NO_COLOR beats FORCE_COLOR", Options{}, env("NO_COLOR=1", "FORCE_COLOR=1"), allTTY, "linux",
			want{colorprofile.Ascii, colorprofile.Ascii, GlyphsFull, true, true, true}},
		{"--color=always on a pipe", Options{Color: ColorAlways}, env(), pipeOut, "linux",
			want{colorprofile.ANSI256, colorprofile.ANSI256, GlyphsFull, true, false, true}},
		{"TERM=dumb", Options{}, []string{"TERM=dumb", "LANG=en_US.UTF-8"}, allTTY, "linux",
			want{colorprofile.NoTTY, colorprofile.NoTTY, GlyphsASCII, false, false, true}},
		{"CI=true", Options{}, env("CI=true"), allTTY, "linux",
			want{colorprofile.NoTTY, colorprofile.NoTTY, GlyphsFull, false, false, false}},
		{"CI=false", Options{}, env("CI=false"), allTTY, "linux", full},
		{"GITHUB_ACTIONS", Options{}, env("GITHUB_ACTIONS=true"), allTTY, "linux",
			want{colorprofile.NoTTY, colorprofile.NoTTY, GlyphsFull, false, false, false}},
		{"--ci", Options{CI: true}, env(), allTTY, "linux",
			want{colorprofile.NoTTY, colorprofile.NoTTY, GlyphsFull, false, false, false}},
		{"--plain", Options{Plain: true}, env(), allTTY, "linux",
			want{colorprofile.NoTTY, colorprofile.NoTTY, GlyphsASCII, false, false, true}},
		{"--plain with --color=always", Options{Plain: true, Color: ColorAlways}, env(), allTTY, "linux",
			want{colorprofile.ANSI256, colorprofile.ANSI256, GlyphsASCII, false, false, true}},
		{"DUES_PLAIN", Options{}, env("DUES_PLAIN=1"), allTTY, "linux",
			want{colorprofile.NoTTY, colorprofile.NoTTY, GlyphsASCII, false, false, true}},
		{"--no-fun", Options{NoFun: true}, env(), allTTY, "linux",
			want{colorprofile.ANSI256, colorprofile.ANSI256, GlyphsFull, true, false, true}},
		{"DUES_NO_FUN", Options{}, env("DUES_NO_FUN=1"), allTTY, "linux",
			want{colorprofile.ANSI256, colorprofile.ANSI256, GlyphsFull, true, false, true}},
		{"stdin not a terminal", Options{}, env(), noStdin, "linux",
			want{colorprofile.ANSI256, colorprofile.ANSI256, GlyphsFull, true, false, false}},
		{"LANG=C", Options{}, []string{"TERM=xterm-256color", "LANG=C"}, allTTY, "linux",
			want{colorprofile.ANSI256, colorprofile.ANSI256, GlyphsASCII, true, true, true}},
		{"no locale", Options{}, []string{"TERM=xterm-256color"}, allTTY, "linux",
			want{colorprofile.ANSI256, colorprofile.ANSI256, GlyphsASCII, true, true, true}},
		{"no locale on macOS", Options{}, []string{"TERM=xterm-256color"}, allTTY, "darwin", full},
		{"LC_ALL wins", Options{}, env("LC_ALL=C"), allTTY, "linux",
			want{colorprofile.ANSI256, colorprofile.ANSI256, GlyphsASCII, true, true, true}},
		{"Japanese locale", Options{}, []string{"TERM=xterm-256color", "LANG=ja_JP.UTF-8"}, allTTY, "linux",
			want{colorprofile.ANSI256, colorprofile.ANSI256, GlyphsASCII, true, true, true}},
		{"RUNEWIDTH_EASTASIAN=0", Options{}, []string{"TERM=xterm-256color", "LANG=ja_JP.UTF-8", "RUNEWIDTH_EASTASIAN=0"}, allTTY, "linux", full},
		{"console host", Options{}, []string{"TERM=xterm-256color"}, allTTY, "windows",
			want{colorprofile.ANSI256, colorprofile.ANSI256, GlyphsConsole, true, true, true}},
		{"Windows Terminal", Options{}, []string{"TERM=xterm-256color", "WT_SESSION=1"}, allTTY, "windows",
			want{colorprofile.TrueColor, colorprofile.TrueColor, GlyphsFull, true, true, true}},
		{"VS Code on Windows", Options{}, []string{"TERM=xterm-256color", "TERM_PROGRAM=vscode"}, allTTY, "windows", full},
		// DUES folds names on Windows; colorprofile does not, but terminals
		// set these variables in upper case anyway.
		{"Windows names fold", Options{}, []string{"TERM=xterm-256color", "wt_session=1"}, allTTY, "windows", full},
		{"Linux names do not fold", Options{}, env("ci=true"), allTTY, "linux", full},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := Resolve(c.o, c.env, c.t, c.goos)
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
	if Resolve(Options{}, env(), allTTY, "linux").Fun {
		t.Fatal("fun on in a build with funDefault=off")
	}
}

func TestTheme(t *testing.T) {
	cases := map[string]Theme{
		"":                                  ThemeUnknown,
		"DUES_THEME=light":                  ThemeLight,
		"DUES_THEME=DARK":                   ThemeDark,
		"COLORFGBG=15;0":                    ThemeDark,
		"COLORFGBG=0;15":                    ThemeLight,
		"COLORFGBG=0;default;7":             ThemeLight,
		"COLORFGBG=7;8":                     ThemeDark,
		"COLORFGBG=default":                 ThemeUnknown,
		"DUES_THEME=dark\x00COLORFGBG=0;15": ThemeDark,
	}
	for kv, want := range cases {
		var e []string
		for _, s := range splitNUL(kv) {
			if s != "" {
				e = append(e, s)
			}
		}
		if got := Resolve(Options{}, env(e...), allTTY, "linux").Theme; got != want {
			t.Errorf("%q: theme %d, want %d", kv, got, want)
		}
	}
}

func splitNUL(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// Taskbar progress goes only to terminals that show it, never under tmux or
// screen, another terminal program, CI, a piped stderr or --plain.
func TestTaskbarGate(t *testing.T) {
	stderrPiped := console.Terminal{InTTY: true, OutTTY: true}
	for _, c := range []struct {
		env  []string
		tty  console.Terminal
		want bool
	}{
		{[]string{"WT_SESSION=x"}, allTTY, true},
		{[]string{"ConEmuANSI=ON"}, allTTY, true},
		{[]string{"ConEmuANSI=OFF"}, allTTY, false},
		{nil, allTTY, false},
		{[]string{"WT_SESSION=x", "TERM_PROGRAM=vscode"}, allTTY, false},
		{[]string{"WT_SESSION=x", "TMUX=/tmp/tmux-1000/default,1,0"}, allTTY, false},
		{[]string{"WT_SESSION=x", "STY=1234.pts-0.host"}, allTTY, false},
		{[]string{"WT_SESSION=x", "CI=true"}, allTTY, false},
		{[]string{"WT_SESSION=x", "TERM=dumb"}, allTTY, false},
		{[]string{"WT_SESSION=x"}, stderrPiped, false},
	} {
		if m := Resolve(Options{}, c.env, c.tty, "windows"); m.Taskbar != c.want {
			t.Errorf("%q, %+v: Taskbar %v, want %v", c.env, c.tty, m.Taskbar, c.want)
		}
	}
	if m := Resolve(Options{Plain: true}, []string{"WT_SESSION=x"}, allTTY, "windows"); m.Taskbar {
		t.Error("taskbar progress under --plain")
	}
}
