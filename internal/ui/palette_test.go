package ui

import (
	"image/color"
	"testing"

	"indicer/internal/testutil/wcag"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// TestPaletteContrast holds the palettes to their promises: the dark and
// light values reach WCAG AA (4.5:1) on their backgrounds, the neutral
// values 3:1 on all of them, in truecolor and as the nearest xterm-256
// colour a 256-colour terminal shows instead.
func TestPaletteContrast(t *testing.T) {
	all := append(append([]string{}, wcag.DarkBackgrounds...), wcag.LightBackgrounds...)
	for r := Info; r < roleCount; r++ {
		for _, c := range []struct {
			theme Theme
			bgs   []string
			min   float64
		}{
			{ThemeDark, wcag.DarkBackgrounds, 4.5},
			{ThemeLight, wcag.LightBackgrounds, 4.5},
			{ThemeUnknown, all, 3.0},
		} {
			fg := r.RGB(c.theme)
			fg256 := ansi.IndexedColor(ansi256(fg))
			for _, bg := range c.bgs {
				b := wcag.RGB(bg)
				if got := wcag.Contrast(fg, b); got < c.min {
					t.Errorf("role %d theme %d: %s on %s is %.2f:1, want >= %.1f", r, c.theme, r.Hex(c.theme), bg, got, c.min)
				}
				if got := wcag.Contrast(fg256, b); got < c.min {
					t.Errorf("role %d theme %d: xterm-256 colour %d (from %s) on %s is %.2f:1, want >= %.1f",
						r, c.theme, int(fg256), r.Hex(c.theme), bg, got, c.min)
				}
			}
		}
	}
}

func TestSGR(t *testing.T) {
	want16 := map[Role]string{Info: "94", Success: "32", Warning: "33", Error: "91", Highlight: "36", Forensics: "95", Evidence: "93", Muted: "90"}
	for r, w := range want16 {
		if got := sgr(colorprofile.ANSI, ThemeDark, r); got != w {
			t.Errorf("ANSI sgr(%d) = %q, want %q", r, got, w)
		}
	}
	if got := sgr(colorprofile.TrueColor, ThemeDark, Info); got != "38;2;108;168;255" {
		t.Errorf("truecolor info = %q", got)
	}
	if got := sgr(colorprofile.ANSI256, ThemeLight, Error); len(got) < 6 || got[:5] != "38;5;" {
		t.Errorf("ANSI256 error = %q", got)
	}
	for _, p := range []colorprofile.Profile{colorprofile.Unknown, colorprofile.NoTTY, colorprofile.Ascii} {
		if got := sgr(p, ThemeDark, Info); got != "" {
			t.Errorf("profile %v gives colour %q", p, got)
		}
	}
	if got := sgr(colorprofile.TrueColor, ThemeDark, Plain); got != "" {
		t.Errorf("Plain has colour %q", got)
	}
	if got := sgr(colorprofile.TrueColor, ThemeUnknown, Muted); got != "" {
		t.Errorf("muted with the neutral palette has colour %q", got)
	}
}

func TestHexAndRGB(t *testing.T) {
	if Error.Hex(ThemeLight) != "#B8222A" || Plain.Hex(ThemeDark) != "" {
		t.Fatal("Hex mismatch")
	}
	if c := Info.RGB(ThemeDark); c != (color.RGBA{0x6C, 0xA8, 0xFF, 0xff}) {
		t.Fatalf("RGB = %v", c)
	}
}
