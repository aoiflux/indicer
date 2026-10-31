package ui

import (
	"fmt"
	"io"
	"runtime"
	"strings"

	"indicer/internal/console"

	"github.com/charmbracelet/x/ansi"
)

// Build says which DUES this is.
type Build struct {
	Version, Codename string
	Commit            string // short commit, empty when the build did not record one
}

// Product is the product's full name.
const Product = "Deduplicated Unified Evidence Store"

// wordmark is the DUES logo, three rows, in box drawing and in ASCII.
var wordmark = map[bool][3]string{
	true: {
		"┌─╮ ╷ ╷ ┌─╴ ╭─╴",
		"│ │ │ │ ├─  ╰─╮",
		"└─╯ ╰─╯ └─╴ ╶─╯",
	},
	false: {
		"+-. . . +-- .--",
		"| | | | |-  '-.",
		"+-' '-' +-- --'",
	},
}

// bannerWidth is the widest the banner may be.
const bannerWidth = 60

// Banner writes the DUES banner to w when w is a terminal outside --plain,
// and reports whether it did. The wordmark sits beside the product name,
// version and build; under 60 columns the wordmark shows alone.
func Banner(w io.Writer, b Build) bool {
	p := For(w)
	if !p.Decorated() {
		return false
	}
	g := p.GlyphSet()
	mark := wordmark[g == GlyphsFull]
	side := bannerSide(b, g.Glyphs())
	width, _ := console.Size()
	for i, row := range mark {
		line := p.Strong(Forensics, row)
		if width >= bannerWidth {
			text := side[i]
			switch i {
			case 0:
				text = p.Bold(text)
			default:
				text = p.Paint(Muted, text)
			}
			line += "   " + text
		}
		fmt.Fprintln(w, line)
	}
	return true
}

// bannerSide is the text beside the wordmark.
func bannerSide(b Build, g Glyphs) [3]string {
	version := "v" + b.Version
	if b.Codename != "" {
		version += fmt.Sprintf(" %q", b.Codename)
	}
	build := []string{runtime.GOOS + "/" + runtime.GOARCH, runtime.Version()}
	if b.Commit != "" {
		build = append([]string{b.Commit}, build...)
	}
	return [3]string{Product, version, strings.Join(build, " "+g.Sep+" ")}
}

// bannerFits reports the banner's size, for tests.
func bannerFits(b Build, g GlyphSet) (rows, cols int) {
	mark := wordmark[g == GlyphsFull]
	side := bannerSide(b, g.Glyphs())
	for i, row := range mark {
		cols = max(cols, ansi.StringWidth(row+"   "+side[i]))
	}
	return len(mark), cols
}
