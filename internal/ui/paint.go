package ui

import (
	"io"

	"indicer/internal/console"

	"github.com/charmbracelet/colorprofile"
)

// Painter styles text for one destination. Colour is applied where text
// is written, never stored in report strings: the TUI reads reports from a
// buffer and would show escape codes as text. The zero Painter adds
// nothing and uses ASCII marks.
type Painter struct {
	profile colorprofile.Profile
	theme   Theme
	glyphs  GlyphSet
	// decorated is a terminal stream outside --plain: marks and layout
	// instead of the "dues: label:" lines scripts read.
	decorated bool
}

// For returns the painter for text written to w. Only console's stdout and
// stderr sinks get colour, and only when their stream may show it; any
// other writer (a buffer, a file, a test capture) gets plain text.
func For(w io.Writer) Painter {
	m := Current()
	var c Caps
	switch w {
	case console.Out():
		c = m.Out
	case console.Err(), console.Progress():
		c = m.Err
	default:
		return Painter{}
	}
	return Painter{profile: c.Profile, theme: m.Theme, glyphs: c.Glyphs, decorated: c.TTY && !m.Plain}
}

// Colour reports whether the painter emits colour codes.
func (p Painter) Colour() bool { return p.profile >= colorprofile.ANSI }

// Decorated reports whether the destination is a terminal outside --plain.
func (p Painter) Decorated() bool { return p.decorated }

// Glyphs returns the marks the destination can show.
func (p Painter) Glyphs() Glyphs { return p.glyphs.Glyphs() }

// GlyphSet returns the destination's glyph set.
func (p Painter) GlyphSet() GlyphSet { return p.glyphs }

// Paint colours s in role r. With the neutral palette coloured text is also
// bold, because that palette is tuned for bold tokens.
func (p Painter) Paint(r Role, s string) string {
	return p.style(r, p.theme == ThemeUnknown && sgr(p.profile, p.theme, r) != "", s)
}

// Strong is Paint in bold: labels, marks and headings.
func (p Painter) Strong(r Role, s string) string { return p.style(r, true, s) }

// Bold is bold text in the terminal's own colour.
func (p Painter) Bold(s string) string { return p.style(Plain, true, s) }

// Faint is secondary text that only passes by: the detail rows under a live
// progress display, a bar's empty track. On a known background it is the
// muted colour. On an unknown one it is faint (SGR 2), which dims the
// terminal's own colour and so is never invisible; a terminal that ignores
// it shows normal text.
func (p Painter) Faint(s string) string {
	if s == "" || p.profile < colorprofile.ASCII {
		return s
	}
	codes := "2"
	if p.theme != ThemeUnknown {
		if c := sgr(p.profile, p.theme, Muted); c != "" {
			codes = c
		}
	}
	return "\x1b[" + codes + "m" + s + "\x1b[0m"
}

func (p Painter) style(r Role, bold bool, s string) string {
	if s == "" || p.profile < colorprofile.ASCII {
		return s
	}
	codes := sgr(p.profile, p.theme, r)
	if bold {
		if codes == "" {
			codes = "1"
		} else {
			codes = "1;" + codes
		}
	}
	if codes == "" {
		return s
	}
	return "\x1b[" + codes + "m" + s + "\x1b[0m"
}
