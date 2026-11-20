// Package wcag holds colours to WCAG 2 contrast in tests: the CLI's palette
// and the TUI's styles are checked against the same reference backgrounds.
package wcag

import (
	"fmt"
	"image/color"
	"math"
)

// Reference terminal backgrounds: black, Windows Terminal Campbell, VS Code,
// One Dark, Solarized dark, Ubuntu; then white, near-whites, Solarized light
// and its base2.
var (
	DarkBackgrounds  = []string{"#000000", "#0C0C0C", "#1E1E1E", "#282C34", "#002B36", "#300A24"}
	LightBackgrounds = []string{"#FFFFFF", "#FAFAFA", "#F2F2F2", "#FDF6E3", "#EEE8D5"}
)

// RGB is the colour "#RRGGBB" names. It panics on anything else: the
// colours are the tests' own.
func RGB(hex string) color.RGBA {
	var r, g, b uint8
	if _, err := fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b); err != nil || len(hex) != 7 {
		panic(fmt.Sprintf("wcag: bad colour %q", hex))
	}
	return color.RGBA{r, g, b, 0xff}
}

// Luminance is a colour's relative luminance.
func Luminance(c color.Color) float64 {
	r, g, b, _ := c.RGBA()
	lin := func(v uint32) float64 {
		s := float64(v>>8) / 255
		if s <= 0.04045 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

// Contrast is the WCAG 2 contrast ratio of two colours, from 1 to 21.
func Contrast(a, b color.Color) float64 {
	la, lb := Luminance(a), Luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}
