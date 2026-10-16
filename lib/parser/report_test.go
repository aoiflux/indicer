package parser

import (
	"bytes"
	"strings"
	"testing"

	"indicer/internal/console"

	"github.com/fatih/color"
)

// TestReportNeutralisesEvidenceStrings is the PAR-24 regression: a partition
// name is evidence data, and printing it raw lets a crafted image drive the
// terminal (clear the screen, rewrite earlier lines, hide text).
func TestReportNeutralisesEvidenceStrings(t *testing.T) {
	prev := color.NoColor
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = prev; console.Reset() })
	var out bytes.Buffer
	console.Configure(&out, &out)

	evil := "EFI" + string(rune(0x1b)) + "[2J" + string(rune(0x202e)) + "txt.exe" + string(rune(0x07))
	r := &indexReport{}
	r.info("partition 1", evil+" (fat)")
	r.warning("note"+string(rune(0x1b))+"]0;title", "x")
	r.print()

	got := out.String()
	for _, bad := range []rune{0x1b, 0x202e, 0x07} {
		if strings.ContainsRune(got, bad) {
			t.Fatalf("report printed control character %U: %q", bad, got)
		}
	}
	if !strings.Contains(got, "EFI") || !strings.Contains(got, "txt.exe") {
		t.Fatalf("report lost the visible text: %q", got)
	}
}
