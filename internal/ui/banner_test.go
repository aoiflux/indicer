package ui

import (
	"bytes"
	"strings"
	"testing"

	"indicer/internal/console"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

var testBuild = Build{Version: "0.39.0", Codename: "<pineapple> spacebar", Commit: "42e2ded-dirty"}

// The banner stays within 6 rows and 60 columns, and within the terminal,
// in every glyph set, for every build, at every width.
func TestBannerSize(t *testing.T) {
	builds := []Build{testBuild, {Version: "0.39.0", Codename: "<pineapple> spacebar", Commit: "74431db"}, {Version: "0.39.0"},
		{Version: "0.39.0", Codename: strings.Repeat("long ", 20), Commit: strings.Repeat("c", 50)}}
	for _, g := range []GlyphSet{GlyphsFull, GlyphsConsole, GlyphsASCII} {
		for _, b := range builds {
			for _, width := range []int{120, 61, 60, 59, 50, 23, 16} {
				rows := bannerLines(b, g, width)
				for i, r := range rows {
					if w := ansi.StringWidth(r.String()); w > bannerWidth || w > max(width-1, 15) {
						t.Errorf("set %d, %+v, width %d: row %d is %d wide: %q", g, b, width, i, w, r)
					}
					if (r.emblem != "") != (width >= 23) || (r.side != "") != (width >= 61) {
						t.Errorf("set %d, width %d: row %d %+v", g, width, i, r)
					}
				}
				if width >= 61 && !strings.Contains(rows[2].side, b.Commit[:min(len(b.Commit), 7)]) {
					t.Errorf("set %d, %+v: build line %q lacks the commit", g, b, rows[2].side)
				}
			}
		}
	}
}

func TestBanner(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	out, _ := terminal(t, ttyMode(colorprofile.TrueColor, ThemeDark, GlyphsFull))
	if !Banner(console.Out(), testBuild) {
		t.Fatal("no banner on a terminal")
	}
	got := ansi.Strip(out.String())
	for _, want := range []string{
		"●─●─●  ┌─╮ ╷ ╷ ┌─╴ ╭─╴  Deduplicated Unified Evidence Store",
		" ╲│╱   │ │ │ │ ├─  ╰─╮  v0.39.0 \"<pineapple> spacebar\"",
		"  ●    └─╯ ╰─╯ └─╴ ╶─╯  42e2ded-dirty · ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("banner lacks %q:\n%s", want, got)
		}
	}
	if strings.ContainsAny(got, "{}") {
		t.Errorf("banner has a brace:\n%s", got)
	}

	// Narrow: the emblem and wordmark alone.
	t.Setenv("COLUMNS", "50")
	out.Reset()
	Banner(console.Out(), testBuild)
	if got := ansi.Strip(out.String()); strings.Contains(got, Product) || strings.Count(got, "\n") != 3 || !strings.Contains(got, "●─●─●  ┌─╮") {
		t.Errorf("narrow banner:\n%s", got)
	}

	// The ASCII and console sets draw both in ASCII.
	for _, g := range []GlyphSet{GlyphsASCII, GlyphsConsole} {
		out, _ = terminal(t, ttyMode(colorprofile.NoTTY, ThemeUnknown, g))
		t.Setenv("COLUMNS", "100")
		Banner(console.Out(), testBuild)
		if got := out.String(); !strings.Contains(got, "o-o-o  +-. . . +-- .--") || !strings.Contains(got, ` \|/   | | | |`) || strings.Contains(got, "\x1b") {
			t.Errorf("set %d banner:\n%q", g, got)
		}
	}
}

func TestFitFields(t *testing.T) {
	fields := []string{"42e2ded-dirty", "windows/amd64", "go1.27.3"}
	for room, want := range map[int]string{
		40: "42e2ded-dirty · windows/amd64 · go1.27.3",
		36: "42e2ded-dirty · windows/amd64",
		20: "42e2ded-dirty",
		5:  "42e2d",
	} {
		if got := fitFields(fields, " · ", room); got != want {
			t.Errorf("room %d: %q, want %q", room, got, want)
		}
	}
}

func TestIdentity(t *testing.T) {
	for _, c := range []struct {
		b    Build
		want string
	}{
		{testBuild, "DUES 0.39.0+42e2ded-dirty (<pineapple> spacebar) - Deduplicated Unified Evidence Store"},
		{Build{Version: "0.39.0", Codename: "<pineapple> spacebar"}, "DUES 0.39.0 (<pineapple> spacebar) - Deduplicated Unified Evidence Store"},
		{Build{Version: "1.0.0"}, "DUES 1.0.0 - Deduplicated Unified Evidence Store"},
	} {
		if got := Identity(c.b); got != c.want {
			t.Errorf("Identity(%+v) = %q, want %q", c.b, got, c.want)
		}
	}
}

func TestNoBannerOnAPipe(t *testing.T) {
	var buf bytes.Buffer
	if Banner(&buf, testBuild) || buf.Len() != 0 {
		t.Fatalf("banner on a pipe: %q", buf.String())
	}
	m := ttyMode(colorprofile.NoTTY, ThemeUnknown, GlyphsASCII)
	m.Plain = true
	out, _ := terminal(t, m)
	if Banner(console.Out(), testBuild) || out.Len() != 0 {
		t.Fatalf("banner under --plain: %q", out.String())
	}
}
