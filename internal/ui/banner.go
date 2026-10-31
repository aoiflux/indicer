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

// Identity is DUES in one line, for output that is not a terminal:
// "DUES 0.39.0+42e2ded (<pineapple> spacebar) - Deduplicated Unified
// Evidence Store". The commit is semver build metadata, left out when the
// build did not record one.
func Identity(b Build) string {
	v := b.Version
	if b.Commit != "" {
		v += "+" + b.Commit
	}
	s := "DUES " + v
	if b.Codename != "" {
		s += " (" + b.Codename + ")"
	}
	return s + " - " + Product
}

// emblem is the relationship graph that opens the banner: three nodes
// joined to one, in box drawing and in ASCII. The console set takes the
// ASCII one: CP437 has no ●, ╲ or ╱.
var emblem = map[bool][3]string{
	true:  {"●─●─●", " ╲│╱ ", "  ●  "},
	false: {"o-o-o", ` \|/ `, "  o  "},
}

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

const (
	// bannerWidth is the widest the banner may be, and the terminal width
	// it needs for the text beside the art.
	bannerWidth = 60
	// artWidth is the emblem, a gutter and the wordmark: the width the
	// emblem needs, with a column to spare.
	artWidth = 5 + 2 + 15
	// sideWidth is the room for the text beside the art.
	sideWidth = bannerWidth - artWidth - 2
)

// bannerRow is one row of the banner, unpainted: the emblem, the wordmark
// and the text beside them. The emblem and the text are "" when the
// terminal is too narrow for them.
type bannerRow struct{ emblem, mark, side string }

// bannerLines lays the banner out for a terminal width columns wide, with a
// column to spare so no row wraps: all of it from 61 columns, the emblem
// and wordmark from 23, the wordmark alone below that.
func bannerLines(b Build, g GlyphSet, width int) [3]bannerRow {
	full := g == GlyphsFull
	side := bannerSide(b, g.Glyphs())
	var rows [3]bannerRow
	for i := range rows {
		rows[i].mark = wordmark[full][i]
		if width > artWidth {
			rows[i].emblem = emblem[full][i]
		}
		if width > bannerWidth {
			rows[i].side = side[i]
		}
	}
	return rows
}

func (r bannerRow) String() string {
	s := r.mark
	if r.emblem != "" {
		s = r.emblem + "  " + s
	}
	if r.side != "" {
		s += "  " + r.side
	}
	return s
}

// Banner writes the DUES banner to w when w is a terminal outside --plain,
// and reports whether it did: the emblem, the wordmark, and beside them the
// product name, the version and the build.
func Banner(w io.Writer, b Build) bool {
	p := For(w)
	if !p.Decorated() {
		return false
	}
	width, _ := console.Size()
	for i, r := range bannerLines(b, p.GlyphSet(), width) {
		line := p.Strong(Forensics, r.mark)
		if r.emblem != "" {
			line = paintEmblem(p, r.emblem) + "  " + line
		}
		if r.side != "" {
			text := p.Paint(Muted, r.side)
			if i == 0 {
				text = p.Bold(r.side)
			}
			line += "  " + text
		}
		fmt.Fprintln(w, line)
	}
	return true
}

// paintEmblem colours the graph: nodes as evidence, edges muted.
func paintEmblem(p Painter, s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case ' ':
			b.WriteRune(r)
		case '●', 'o':
			b.WriteString(p.Strong(Evidence, string(r)))
		default:
			b.WriteString(p.Paint(Muted, string(r)))
		}
	}
	return b.String()
}

// bannerSide is the text beside the art: the product, the version and
// codename, and the build (commit, platform, Go version) as far as it fits.
func bannerSide(b Build, g Glyphs) [3]string {
	version := "v" + b.Version
	if b.Codename != "" {
		version += fmt.Sprintf(" %q", b.Codename)
	}
	var build []string
	if b.Commit != "" {
		build = append(build, b.Commit)
	}
	build = append(build, runtime.GOOS+"/"+runtime.GOARCH, runtime.Version())
	return [3]string{Product, ansi.Truncate(version, sideWidth, g.Ellipsis), fitFields(build, " "+g.Sep+" ", sideWidth)}
}

// fitFields joins fields with sep while they fit in room columns; those
// that do not are left out from the end. A first field too long on its own
// is cut.
func fitFields(fields []string, sep string, room int) string {
	out := ""
	for i, f := range fields {
		next := f
		if i > 0 {
			next = out + sep + f
		}
		if ansi.StringWidth(next) > room {
			if i == 0 {
				return ansi.Truncate(f, room, "")
			}
			break
		}
		out = next
	}
	return out
}
