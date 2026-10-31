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

// The banner stays within 6 rows and 60 columns in every glyph set, with
// the longest commit form.
func TestBannerSize(t *testing.T) {
	for _, g := range []GlyphSet{GlyphsFull, GlyphsConsole, GlyphsASCII} {
		rows, cols := bannerFits(testBuild, g)
		if rows > 6 || cols > bannerWidth {
			t.Errorf("set %d: banner is %d rows by %d columns", g, rows, cols)
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
	for _, want := range []string{"┌─╮ ╷ ╷ ┌─╴ ╭─╴   Deduplicated Unified Evidence Store", `v0.39.0 "<pineapple> spacebar"`, "42e2ded-dirty · "} {
		if !strings.Contains(got, want) {
			t.Errorf("banner lacks %q:\n%s", want, got)
		}
	}
	if strings.ContainsAny(got, "{}") {
		t.Errorf("banner has a brace:\n%s", got)
	}

	// Narrow: the wordmark alone.
	t.Setenv("COLUMNS", "50")
	out.Reset()
	Banner(console.Out(), testBuild)
	if got := ansi.Strip(out.String()); strings.Contains(got, Product) || strings.Count(got, "\n") != 3 {
		t.Errorf("narrow banner:\n%s", got)
	}

	// ASCII set.
	out, _ = terminal(t, ttyMode(colorprofile.NoTTY, ThemeUnknown, GlyphsASCII))
	t.Setenv("COLUMNS", "100")
	Banner(console.Out(), testBuild)
	if got := out.String(); !strings.Contains(got, "+-. . . +-- .--") || strings.Contains(got, "\x1b") {
		t.Errorf("ASCII banner:\n%q", got)
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
