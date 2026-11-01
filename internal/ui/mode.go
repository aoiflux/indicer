// Package ui is how DUES looks in a terminal: colour, glyphs, the banner,
// progress and the small extras.
//
// It decides once per process what each standard stream may show, from
// the command line and what internal/console reports about the streams.
// It reads no environment variable: NO_COLOR, TERM, CI and the like are
// replaced by --color, --plain and --ci, so the same command line looks
// the same everywhere. The CLI never asks the terminal anything either:
// queries are slow over SSH and leave stray bytes in the shell. It writes
// only through internal/console, so the TUI, the FFI library and the
// compression product can still redirect or silence it.
package ui

import (
	"strings"

	"indicer/internal/console"

	"github.com/charmbracelet/colorprofile"
)

// Flag names, shared by PreScan and the kingpin definition.
const (
	FlagColor   = "color"
	FlagPlain   = "plain"
	FlagCI      = "ci"
	FlagNoFun   = "no-fun"
	FlagTheme   = "theme"
	FlagTaskbar = "taskbar"
)

// ColorPolicy is the --color setting.
type ColorPolicy string

const (
	// ColorAuto is 256 colours on a terminal and none elsewhere.
	ColorAuto   ColorPolicy = "auto"
	ColorAlways ColorPolicy = "always"
	ColorNever  ColorPolicy = "never"
	// ColorANSI keeps colour but uses only the terminal theme's own 16
	// colours, so high-contrast and colour-blind themes stay in charge.
	ColorANSI ColorPolicy = "ansi"
	// ColorTrueColor is 24-bit colour on a terminal.
	ColorTrueColor ColorPolicy = "truecolor"
)

// ColorPolicies lists the accepted --color values.
var ColorPolicies = []string{string(ColorAuto), string(ColorAlways), string(ColorNever), string(ColorANSI), string(ColorTrueColor)}

func parseColorPolicy(s string) ColorPolicy {
	switch p := ColorPolicy(strings.ToLower(strings.TrimSpace(s))); p {
	case ColorAuto, ColorAlways, ColorNever, ColorANSI, ColorTrueColor:
		return p
	}
	return ""
}

// Theme is the terminal background, when DUES is told it.
type Theme uint8

const (
	// ThemeUnknown (--theme auto) uses the neutral palette, readable on
	// dark and light. The TUI asks the terminal instead.
	ThemeUnknown Theme = iota
	ThemeDark
	ThemeLight
)

// ThemeNames lists the accepted --theme values.
var ThemeNames = []string{"auto", "dark", "light"}

func parseTheme(s string) Theme {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "dark":
		return ThemeDark
	case "light":
		return ThemeLight
	}
	return ThemeUnknown
}

// Options are the presentation switches given on the command line.
type Options struct {
	Color   ColorPolicy // empty when --color was not given
	Plain   bool        // --plain: no colour, ASCII, no animation
	CI      bool        // --ci: behave as if no terminal is attached
	NoFun   bool        // --no-fun: no easter eggs
	Theme   Theme       // --theme; ThemeUnknown for auto
	Taskbar bool        // --taskbar: progress on the terminal's tab (OSC 9;4)
}

// PreScan reads the presentation flags from the raw arguments and returns
// the arguments without them. Help and version text print before kingpin
// parses (main's maybeHandleHelp), and must already honour the flags;
// kingpin accepts the same flags later and rejects bad values.
func PreScan(args []string) (Options, []string) {
	var o Options
	rest := make([]string, 0, len(args))
	// value reads a flag's value, given as --name=V or --name V.
	value := func(i *int, a, name string) (string, bool) {
		if v, ok := strings.CutPrefix(a, "--"+name+"="); ok {
			return v, true
		}
		if a == "--"+name {
			if *i+1 < len(args) {
				*i++
				return args[*i], true
			}
			return "", true
		}
		return "", false
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		switch a {
		case "--" + FlagPlain:
			o.Plain = true
		case "--no-" + FlagPlain:
			o.Plain = false
		case "--" + FlagCI:
			o.CI = true
		case "--no-" + FlagCI:
			o.CI = false
		case "--" + FlagNoFun:
			o.NoFun = true
		case "--" + FlagTaskbar:
			o.Taskbar = true
		case "--no-" + FlagTaskbar:
			o.Taskbar = false
		default:
			if v, ok := value(&i, a, FlagColor); ok {
				o.Color = parseColorPolicy(v)
			} else if v, ok := value(&i, a, FlagTheme); ok {
				o.Theme = parseTheme(v)
			} else {
				rest = append(rest, a)
			}
		}
	}
	return o, rest
}

// GlyphSet says which characters a stream can show.
type GlyphSet uint8

const (
	// GlyphsASCII is plain ASCII, under --plain: for screen readers, the
	// Windows console host's raster fonts, dumb terminals and terminals
	// that draw box drawing two columns wide.
	GlyphsASCII GlyphSet = iota
	// GlyphsFull is Unicode marks, the braille spinner and box drawing.
	GlyphsFull
)

// Caps is what one output stream may show.
type Caps struct {
	TTY bool // a terminal (false for every stream under --ci)
	// Profile is the colour depth. NoTTY means no escape codes at all
	// (--plain, --color never, pipes).
	Profile colorprofile.Profile
	Glyphs  GlyphSet
	// Motion allows redraws, spinners and animations.
	Motion bool
}

// Mode is what this process may show, decided once at start-up.
type Mode struct {
	Out, Err Caps
	Theme    Theme
	// Interactive is true when prompts may read stdin.
	Interactive bool
	CI          bool
	Plain       bool
	// Fun allows the rare easter eggs. Whether one plays is decided later.
	Fun bool
	// Taskbar allows progress on the terminal's tab and taskbar (OSC 9;4).
	Taskbar bool
}

// funDefault turns the easter eggs off for a whole build:
// go build -ldflags "-X indicer/internal/ui.funDefault=off".
var funDefault = "on"

// Resolve decides the Mode from the flags and the streams' terminal state.
func Resolve(o Options, t console.Terminal) Mode {
	m := Mode{Plain: o.Plain, CI: o.CI, Theme: o.Theme}
	if m.CI {
		t = console.Terminal{}
	}
	glyphs := GlyphsFull
	if m.Plain {
		glyphs = GlyphsASCII
	}
	stream := func(tty bool) Caps {
		return Caps{
			TTY:     tty,
			Profile: profile(o.Color, tty, m.Plain),
			Glyphs:  glyphs,
			Motion:  tty && !m.Plain,
		}
	}
	m.Out, m.Err = stream(t.OutTTY), stream(t.ErrTTY)
	m.Interactive = t.InTTY
	m.Fun = m.Err.Motion && t.InTTY && t.OutTTY && !o.NoFun && funDefault != "off"
	// Only the user knows whether the terminal shows OSC 9;4 (Windows
	// Terminal and ConEmu do; iTerm2 takes it for a notification).
	m.Taskbar = m.Err.Motion && o.Taskbar
	return m
}

// profile picks a stream's colour depth. Nothing in the environment says
// what the terminal can show, so a terminal gets 256 colours, which every
// current terminal and tmux shows: --color truecolor asks for 24-bit and
// --color ansi for the theme's 16. A pipe, --plain and --color never get no
// escape codes; --color always colours a pipe too.
func profile(policy ColorPolicy, tty, plain bool) colorprofile.Profile {
	switch {
	case policy == ColorNever:
		return colorprofile.NoTTY
	case policy == ColorAlways:
		return colorprofile.ANSI256
	case !tty || plain:
		return colorprofile.NoTTY
	case policy == ColorTrueColor:
		return colorprofile.TrueColor
	case policy == ColorANSI:
		return colorprofile.ANSI
	}
	return colorprofile.ANSI256
}
