package ui

import (
	"image/color"
	"strconv"
	"sync"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// Role is what a piece of text means. Colour follows from the role, and a
// role is never the only carrier of meaning: every state also has a mark
// and a word.
type Role uint8

const (
	// Plain is the terminal's own foreground: all sentences.
	Plain Role = iota
	// Info marks neutral notices and hints.
	Info
	// Success marks completed steps and verified hashes.
	Success
	// Warning marks recoverable problems, skipped items and exit 2.
	Warning
	// Error marks failures, verification mismatches and aborts.
	Error
	// Highlight marks text to type, the active step and search hits.
	Highlight
	// Forensics marks domain metadata (filesystem and partition types,
	// artefact kinds, relations) and is the brand accent.
	Forensics
	// Evidence marks evidence identity: names, IDs, hashes, database paths.
	Evidence
	// Muted marks secondary text: units, separators, timings, build data.
	Muted
	roleCount
)

// swatch is one role's colour in each palette. The dark and light values
// reach WCAG AA (4.5:1) on common terminal backgrounds; the neutral values
// all sit at relative luminance 0.20 and reach 3:1 on dark and light
// backgrounds alike, for bold tokens and marks when the background is not
// known. ansi is the 16-colour index used when only the theme's colours
// may show. palette_test.go checks all of it.
type swatch struct {
	dark, light, neutral string
	ansi                 uint8
}

var swatches = [roleCount]swatch{
	Info:      {"#6CA8FF", "#1F5FBF", "#2D7BDB", 12},
	Success:   {"#4FC47F", "#17702F", "#298D4A", 2},
	Warning:   {"#E8C547", "#7A5E00", "#967904", 3},
	Error:     {"#FF6B6B", "#B8222A", "#DD4640", 9},
	Highlight: {"#4CCFD6", "#006A73", "#158995", 6},
	Forensics: {"#B69CFF", "#6B3FC0", "#8F65D8", 13},
	Evidence:  {"#F0A93A", "#8A5300", "#C56C05", 11},
	Muted:     {"#9DA3AD", "#575E68", "#747D88", 8},
}

// Hex is the role's truecolor value for a theme, for callers that style
// with their own library (the TUI). Plain has none.
func (r Role) Hex(t Theme) string {
	if r == Plain || r >= roleCount {
		return ""
	}
	s := swatches[r]
	switch t {
	case ThemeDark:
		return s.dark
	case ThemeLight:
		return s.light
	}
	return s.neutral
}

// RGB is Hex as a colour.
func (r Role) RGB(t Theme) color.RGBA {
	h := r.Hex(t)
	if len(h) != 7 {
		return color.RGBA{}
	}
	v, _ := strconv.ParseUint(h[1:], 16, 32)
	return color.RGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xff}
}

// colours reports whether r is drawn in colour for theme t. With the
// neutral palette, muted text stays in the terminal's own colour: its
// neutral value reaches only 3:1, which is enough for bold marks but not
// for running text.
func (r Role) colours(t Theme) bool {
	return r != Plain && r < roleCount && !(r == Muted && t == ThemeUnknown)
}

type sgrKey struct {
	p colorprofile.Profile
	t Theme
	r Role
}

var (
	sgrOnce  sync.Once
	sgrTable map[sgrKey]string
)

// sgr is the SGR parameter list selecting r's colour, or "" for none.
func sgr(p colorprofile.Profile, t Theme, r Role) string {
	if p < colorprofile.ANSI || !r.colours(t) {
		return ""
	}
	sgrOnce.Do(buildSGR)
	return sgrTable[sgrKey{p, t, r}]
}

func buildSGR() {
	sgrTable = make(map[sgrKey]string)
	for r := Info; r < roleCount; r++ {
		for _, t := range []Theme{ThemeUnknown, ThemeDark, ThemeLight} {
			c := r.RGB(t)
			sgrTable[sgrKey{colorprofile.TrueColor, t, r}] = "38;2;" + strconv.Itoa(int(c.R)) + ";" + strconv.Itoa(int(c.G)) + ";" + strconv.Itoa(int(c.B))
			sgrTable[sgrKey{colorprofile.ANSI256, t, r}] = "38;5;" + strconv.Itoa(ansi256(c))
			a := int(swatches[r].ansi)
			if a < 8 {
				sgrTable[sgrKey{colorprofile.ANSI, t, r}] = strconv.Itoa(30 + a)
			} else {
				sgrTable[sgrKey{colorprofile.ANSI, t, r}] = strconv.Itoa(90 + a - 8)
			}
		}
	}
}

// ansi256 is the nearest xterm-256 index, as colorprofile computes it.
func ansi256(c color.Color) int {
	switch v := colorprofile.ANSI256.Convert(c).(type) {
	case ansi.IndexedColor:
		return int(v)
	case ansi.BasicColor:
		return int(v)
	}
	return 7
}
