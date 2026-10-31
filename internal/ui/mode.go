// Package ui is how DUES looks in a terminal: colour, glyphs, the banner,
// progress and the small extras.
//
// It decides once per process what each standard stream may show, from
// the command line, the environment and what internal/console reports
// about the streams. It never asks the terminal itself: background and
// capability queries are slow over SSH and leave stray bytes in the shell.
// It writes only through internal/console, so the TUI, the FFI library
// and the compression product can still redirect or silence it.
package ui

import (
	"slices"
	"strconv"
	"strings"

	"indicer/internal/console"

	"github.com/charmbracelet/colorprofile"
)

// Flag names, shared by PreScan and the kingpin definition.
const (
	FlagColor = "color"
	FlagPlain = "plain"
	FlagCI    = "ci"
	FlagNoFun = "no-fun"
)

// ColorPolicy is the --color setting (or DUES_COLOR).
type ColorPolicy string

const (
	ColorAuto   ColorPolicy = "auto"
	ColorAlways ColorPolicy = "always"
	ColorNever  ColorPolicy = "never"
	// ColorANSI keeps colour but uses only the terminal theme's own 16
	// colours, so high-contrast and colour-blind themes stay in charge.
	ColorANSI ColorPolicy = "ansi"
)

// ColorPolicies lists the accepted --color values.
var ColorPolicies = []string{string(ColorAuto), string(ColorAlways), string(ColorNever), string(ColorANSI)}

func parseColorPolicy(s string) ColorPolicy {
	switch p := ColorPolicy(strings.ToLower(strings.TrimSpace(s))); p {
	case ColorAuto, ColorAlways, ColorNever, ColorANSI:
		return p
	}
	return ""
}

// Options are the presentation switches given on the command line.
type Options struct {
	Color ColorPolicy // empty when --color was not given
	Plain bool        // --plain: no colour, ASCII, no animation
	CI    bool        // --ci: behave as if no terminal is attached
	NoFun bool        // --no-fun: no easter eggs
}

// PreScan reads the presentation flags from the raw arguments and returns
// the arguments without them. Help and version text print before kingpin
// parses (main's maybeHandleHelp), and must already honour the flags;
// kingpin accepts the same flags later and rejects bad values.
func PreScan(args []string) (Options, []string) {
	var o Options
	rest := make([]string, 0, len(args))
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
		case "--" + FlagColor:
			if i+1 < len(args) {
				i++
				o.Color = parseColorPolicy(args[i])
			}
		default:
			if v, ok := strings.CutPrefix(a, "--"+FlagColor+"="); ok {
				o.Color = parseColorPolicy(v)
				continue
			}
			rest = append(rest, a)
		}
	}
	return o, rest
}

// GlyphSet says which characters a stream can show.
type GlyphSet uint8

const (
	// GlyphsASCII is plain ASCII: --plain, dumb terminals, non-UTF-8
	// locales, and East Asian locales, where box drawing is often drawn
	// double width.
	GlyphsASCII GlyphSet = iota
	// GlyphsConsole is the Windows console host without a modern terminal
	// around it: no font fallback, so ASCII marks plus the CP437 blocks.
	GlyphsConsole
	// GlyphsFull is Unicode marks, the braille spinner and box drawing.
	GlyphsFull
)

// Theme is the terminal background, when DUES is told it.
type Theme uint8

const (
	// ThemeUnknown uses the neutral palette, readable on dark and light.
	ThemeUnknown Theme = iota
	ThemeDark
	ThemeLight
)

// Caps is what one output stream may show.
type Caps struct {
	TTY bool // a terminal (false for every stream under --ci and in CI)
	// Profile is the colour depth. NoTTY means no escape codes at all
	// (--plain, pipes); Ascii means no colour but bold is fine (NO_COLOR).
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
}

// funDefault turns the easter eggs off for a whole build:
// go build -ldflags "-X indicer/internal/ui.funDefault=off".
var funDefault = "on"

// ciVariables are set by CI services; any of them means no human watches.
var ciVariables = []string{"CI", "GITHUB_ACTIONS", "GITLAB_CI", "BUILDKITE", "JENKINS_URL", "TF_BUILD", "TEAMCITY_VERSION"}

// Resolve decides the Mode from the flags, the environment (os.Environ
// form), the streams' terminal state and the operating system.
func Resolve(o Options, env []string, t console.Terminal, goos string) Mode {
	e := newEnviron(env, goos)
	m := Mode{Plain: o.Plain || truthy(e.get("DUES_PLAIN"))}
	for _, v := range ciVariables {
		if truthy(e.get(v)) {
			m.CI = true
		}
	}
	m.CI = m.CI || o.CI
	if m.CI {
		t = console.Terminal{}
	}
	dumb := e.get("TERM") == "dumb"

	policy := o.Color
	if policy == "" {
		policy = parseColorPolicy(e.get("DUES_COLOR"))
	}
	glyphs := glyphSet(e, goos, m.Plain || dumb)
	stream := func(tty bool) Caps {
		return Caps{
			TTY:     tty,
			Profile: profile(policy, e, tty, m.Plain),
			Glyphs:  glyphs,
			Motion:  tty && !m.Plain && !dumb,
		}
	}
	m.Out, m.Err = stream(t.OutTTY), stream(t.ErrTTY)
	m.Theme = theme(e)
	m.Interactive = t.InTTY
	m.Fun = m.Err.Motion && t.InTTY && t.OutTTY && !o.NoFun &&
		!truthy(e.get("DUES_NO_FUN")) && funDefault != "off"
	return m
}

// profile picks a stream's colour depth. Precedence: --color, DUES_COLOR,
// --plain (no escape codes at all), NO_COLOR (any value, per no-color.org:
// no colour, bold still allowed), CLICOLOR_FORCE and FORCE_COLOR, then the
// terminal type from the environment.
func profile(policy ColorPolicy, e environ, tty, plain bool) colorprofile.Profile {
	if policy == ColorNever {
		return colorprofile.NoTTY
	}
	forced := policy == ColorAlways
	if !forced {
		if plain {
			return colorprofile.NoTTY
		}
		if e.get("NO_COLOR") != "" {
			if tty {
				return colorprofile.Ascii
			}
			return colorprofile.NoTTY
		}
		forced = truthy(e.get("CLICOLOR_FORCE")) || truthy(e.get("FORCE_COLOR"))
		if !tty && !forced {
			return colorprofile.NoTTY
		}
	}
	// colorprofile.Env reads the terminal type from the environment only.
	// colorprofile.Detect is not used: inside tmux it runs `tmux info` and
	// it reads terminfo files, too slow for every start. NO_COLOR is
	// settled above, and here it must not undo --color=always.
	p := colorprofile.Env(e.without("NO_COLOR"))
	if forced && p < colorprofile.ANSI {
		p = colorprofile.ANSI
	}
	if policy == ColorANSI && p > colorprofile.ANSI {
		p = colorprofile.ANSI
	}
	return p
}

func glyphSet(e environ, goos string, ascii bool) GlyphSet {
	if ascii || eastAsian(e) {
		return GlyphsASCII
	}
	if goos == "windows" {
		// Go writes to consoles in UTF-16, so the locale does not matter;
		// the font does. Windows Terminal, VS Code and ConEmu fall back to
		// other fonts for missing glyphs; the plain console host does not.
		if e.get("WT_SESSION") != "" || e.get("TERM_PROGRAM") != "" || e.get("ConEmuANSI") == "ON" {
			return GlyphsFull
		}
		return GlyphsConsole
	}
	loc := locale(e)
	if loc == "" {
		if goos == "darwin" {
			return GlyphsFull
		}
		return GlyphsASCII
	}
	l := strings.ToLower(loc)
	if strings.Contains(l, "utf-8") || strings.Contains(l, "utf8") {
		return GlyphsFull
	}
	return GlyphsASCII
}

func locale(e environ) string {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := e.get(k); v != "" {
			return v
		}
	}
	return ""
}

// eastAsian follows go-runewidth: RUNEWIDTH_EASTASIAN decides when set,
// otherwise a Chinese, Japanese or Korean locale does. Those terminals
// often draw box-drawing characters two columns wide.
func eastAsian(e environ) bool {
	switch e.get("RUNEWIDTH_EASTASIAN") {
	case "1":
		return true
	case "0":
		return false
	}
	l := strings.ToLower(locale(e))
	return strings.HasPrefix(l, "ja") || strings.HasPrefix(l, "ko") || strings.HasPrefix(l, "zh")
}

// theme reads DUES_THEME, then COLORFGBG ("fg;bg", rxvt convention: bg 0-6
// or 8 is dark, 7 or 9-15 is light).
func theme(e environ) Theme {
	switch strings.ToLower(e.get("DUES_THEME")) {
	case "dark":
		return ThemeDark
	case "light":
		return ThemeLight
	}
	if v := e.get("COLORFGBG"); v != "" {
		bg := v[strings.LastIndexByte(v, ';')+1:]
		if n, err := strconv.Atoi(bg); err == nil && n >= 0 && n <= 15 {
			if n == 7 || n >= 9 {
				return ThemeLight
			}
			return ThemeDark
		}
	}
	return ThemeUnknown
}

func truthy(v string) bool {
	v = strings.TrimSpace(v)
	return v != "" && v != "0" && !strings.EqualFold(v, "false") && !strings.EqualFold(v, "no")
}

// environ is an os.Environ-style list with lookups. Names are compared
// case-insensitively on Windows, as the system does.
type environ struct {
	list []string
	fold bool
}

func newEnviron(env []string, goos string) environ {
	return environ{list: env, fold: goos == "windows"}
}

func (e environ) match(k, name string) bool {
	if e.fold {
		return strings.EqualFold(k, name)
	}
	return k == name
}

// get returns the value of name; the last assignment wins, as in exec.Cmd.
func (e environ) get(name string) string {
	for _, kv := range slices.Backward(e.list) {
		if k, v, ok := strings.Cut(kv, "="); ok && e.match(k, name) {
			return v
		}
	}
	return ""
}

func (e environ) without(name string) []string {
	out := make([]string, 0, len(e.list))
	for _, kv := range e.list {
		if k, _, _ := strings.Cut(kv, "="); !e.match(k, name) {
			out = append(out, kv)
		}
	}
	return out
}
