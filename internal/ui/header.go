package ui

import (
	"fmt"
	"io"
	"strings"

	"indicer/internal/termsafe"
)

// Setting is one effective setting shown under the run header: Phrase on a
// terminal ("zstd best"), Key=Value in plain output ("compress=best").
type Setting struct {
	Key, Value, Phrase string
}

// Header opens a long operation: what runs, on what, into which database,
// and with which settings.
type Header struct {
	Version  string
	Command  string
	Subject  string // evidence path, hash or query; may be empty
	Target   string // database root
	Settings []Setting
}

// RunHeader writes h to w, normally stderr. On a terminal:
//
//	◆ DUES v0.4.0 · store · jean.e01 → O:\cases\c42
//	  performance · zstd best · revrel
//
// In plain output, two "dues: <command>:" lines with key=value settings.
func RunHeader(w io.Writer, h Header) {
	p := For(w)
	subject, target := termsafe.Line(h.Subject), termsafe.Line(h.Target)
	if !p.Decorated() {
		line := "DUES " + h.Version
		if subject != "" {
			line += ", " + subject
		}
		if target != "" {
			line += " -> " + target
		}
		fmt.Fprintf(w, "dues: %s: %s\n", h.Command, line)
		if len(h.Settings) > 0 {
			kv := make([]string, len(h.Settings))
			for i, s := range h.Settings {
				kv[i] = s.Key + "=" + s.Value
			}
			fmt.Fprintf(w, "dues: %s: settings: %s\n", h.Command, strings.Join(kv, " "))
		}
		return
	}
	g := p.Glyphs()
	sep := " " + p.Paint(Muted, g.Sep) + " "
	var b strings.Builder
	b.WriteString(p.Strong(Forensics, g.Brand) + " " + p.Bold("DUES") + " " + p.Paint(Muted, "v"+h.Version))
	b.WriteString(sep + p.Bold(h.Command))
	if subject != "" {
		b.WriteString(sep + p.Paint(Evidence, subject))
	}
	if target != "" {
		b.WriteString(" " + p.Paint(Muted, g.Arrow) + " " + p.Paint(Evidence, target))
	}
	// Broken at spaces to fit the terminal, so a long path never wraps.
	width, _ := termSize()
	fmt.Fprintln(w, wrapLine(b.String(), width-1, "  "))
	if len(h.Settings) > 0 {
		ph := make([]string, len(h.Settings))
		for i, s := range h.Settings {
			ph[i] = s.Phrase
		}
		fmt.Fprintln(w, wrapLine("  "+p.Paint(Muted, strings.Join(ph, " "+g.Sep+" ")), width-1, "  "))
	}
}
