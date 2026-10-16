// Package termsafe makes evidence-derived text safe to print to a terminal.
//
// File names, partition labels and artefact strings come from the evidence,
// which an adversary may have prepared. Printed raw, an escape sequence in a
// file name can recolour the screen, move the cursor, rewrite earlier lines or
// set the window title, and a bidi override can make one name display as
// another. These functions replace every such character with a visible escape.
package termsafe

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// String returns s with every control character, bidi control and invalid
// UTF-8 byte replaced by a visible escape. Newlines and tabs are escaped too,
// so the result always prints on one line.
func String(s string) string { return sanitize(s, false) }

// Line is String, for call sites that print one line per value.
func Line(s string) string { return sanitize(s, false) }

// Block is like String but keeps newlines and tabs, for multi-line text such
// as an extracted document preview.
func Block(s string) string { return sanitize(s, true) }

func sanitize(s string, keepLayout bool) string {
	if isClean(s, keepLayout) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case keepLayout && (r == '\n' || r == '\t'):
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case unsafeRune(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func isClean(s string, keepLayout bool) bool {
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if (c < 0x20 || c == 0x7f) && !(keepLayout && (c == '\n' || c == '\t')) {
				return false
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && size <= 1) || unsafeRune(r) {
			return false
		}
		i += size
	}
	return true
}

// unsafeRune reports C1 controls, which some terminals act on like ESC, and
// the Unicode controls that reorder or hide text.
func unsafeRune(r rune) bool {
	switch {
	case r >= 0x80 && r <= 0x9f: // C1 controls, including CSI (U+009B)
		return true
	case r >= 0x200b && r <= 0x200f: // zero-width characters and LRM/RLM
		return true
	case r >= 0x202a && r <= 0x202e: // embeddings and overrides
		return true
	case r >= 0x2066 && r <= 0x2069: // isolates
		return true
	case r == 0x061c || r == 0xfeff: // Arabic letter mark, BOM
		return true
	}
	return false
}
