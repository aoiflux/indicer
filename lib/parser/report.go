package parser

import (
	"fmt"
	"strings"

	"github.com/fatih/color"
)

type reportEntry struct {
	label string
	warn  bool
	msg   string
}

// indexReport accumulates per-partition info and warnings during IndexFilesystem,
// then prints them as a colour-coded summary box.
type indexReport struct {
	entries []reportEntry
}

func (r *indexReport) info(label, msg string) {
	r.entries = append(r.entries, reportEntry{label: label, msg: msg})
}

func (r *indexReport) warning(label, msg string) {
	r.entries = append(r.entries, reportEntry{label: label, warn: true, msg: msg})
}

func (r *indexReport) print() {
	if len(r.entries) == 0 {
		return
	}

	labelColor := color.New(color.FgCyan, color.Bold)
	warnColor := color.New(color.FgYellow, color.Bold)
	infoColor := color.New(color.FgWhite)
	borderColor := color.New(color.FgHiBlack)

	// Calculate box width from longest line.
	width := 0
	for _, e := range r.entries {
		line := fmt.Sprintf(" %s  %s ", e.label, e.msg)
		if len(line) > width {
			width = len(line)
		}
	}
	if width < 40 {
		width = 40
	}

	border := strings.Repeat("─", width)
	borderColor.Printf("┌%s┐\n", border)
	for _, e := range r.entries {
		line := fmt.Sprintf(" %s  %s ", e.label, e.msg)
		pad := width - len(line)
		borderColor.Print("│")
		if e.warn {
			warnColor.Printf(" %s ", e.label)
		} else {
			labelColor.Printf(" %s ", e.label)
		}
		infoColor.Printf(" %s%s", e.msg, strings.Repeat(" ", pad))
		borderColor.Println("│")
	}
	borderColor.Printf("└%s┘\n", border)
}
