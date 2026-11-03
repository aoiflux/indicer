package ui

import (
	"strings"
	"time"

	"indicer/internal/progress"
	"indicer/internal/termsafe"

	"github.com/charmbracelet/x/ansi"
)

// clean sanitises text from evidence (names, notes, errors) for one line.
func clean(s string) string { return termsafe.Line(s) }

// part is one field of a row; rows drop fields with the lowest keep first
// until they fit the terminal.
type part struct {
	s    string
	keep int // 0 is dropped first; never dropped above keepAlways
}

const keepAlways = 9

// fit joins parts with two spaces into at most width columns. The bar at
// index barAt (drawn by barFn, 10 to 50 cells) takes the room left; when
// it would be under 10 cells, parts with the lowest keep go first, then the
// bar itself.
func fit(parts []part, barAt int, barFn func(int) string, width int) string {
	for {
		others := 0
		for i, pt := range parts {
			if i != barAt && pt.s != "" {
				others += ansi.StringWidth(pt.s) + 2
			}
		}
		barW := 0
		if barFn != nil {
			barW = min(width-others, 50)
			// Extras (keep 1 and below) give way before the bar shrinks
			// under 20 cells; everything droppable gives way before it
			// goes under 10.
			if barW < 20 && dropBelow(parts, barAt, 2) {
				continue
			}
			if barW < 10 {
				if !dropLowest(parts, barAt) {
					barFn = nil
				}
				continue
			}
		}
		out := make([]string, 0, len(parts))
		for i, pt := range parts {
			switch {
			case i == barAt:
				if barFn != nil {
					out = append(out, barFn(barW))
				}
			case pt.s != "":
				out = append(out, pt.s)
			}
		}
		line := strings.Join(out, "  ")
		if ansi.StringWidth(line) <= width || !dropLowest(parts, barAt) {
			return ansi.Truncate(line, width, "")
		}
	}
}

// dropBelow clears the part with the lowest keep under limit; false when
// there is none.
func dropBelow(parts []part, barAt, limit int) bool {
	low := -1
	for i, pt := range parts {
		if i != barAt && pt.s != "" && pt.keep < limit && (low < 0 || pt.keep < parts[low].keep) {
			low = i
		}
	}
	if low < 0 {
		return false
	}
	parts[low].s = ""
	return true
}

// wrapLine breaks s at spaces into lines of at most width columns, the
// following ones indented; a word longer than a line is cut.
func wrapLine(s string, width int, indent string) string {
	if width <= len(indent)+10 || ansi.StringWidth(s) <= width {
		return s
	}
	lines := strings.Split(ansi.Wrap(s, width-len(indent), ""), "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = indent + strings.TrimLeft(lines[i], " ")
	}
	return strings.Join(lines, "\n")
}

// dropLowest clears the droppable part with the lowest keep; false when
// none is left.
func dropLowest(parts []part, barAt int) bool {
	low := -1
	for i, pt := range parts {
		if i == barAt || pt.s == "" || pt.keep >= keepAlways {
			continue
		}
		if low < 0 || pt.keep < parts[low].keep {
			low = i
		}
	}
	if low < 0 {
		return false
	}
	parts[low].s = ""
	return true
}

// stoppedAt says how far a phase got: " at 63% (2.65/4.20 GiB)".
func stoppedAt(v progress.View) string {
	if v.Total <= 0 {
		if v.Unit != progress.None && v.Done > 0 {
			return " after " + fmtAmount(v.Done, v.Unit)
		}
		return ""
	}
	return " at " + itoa(percent(v.Done, v.Total, true)) + "% (" + fmtPair(v.Done, v.Total, v.Unit) + ")"
}

func itoa(n int) string { return fmtCount(int64(n)) }

// plainEnd is the plain line for a phase that ended.
func plainEnd(v progress.View) string {
	d := v.Ended.Sub(v.Started)
	switch v.State {
	case progress.OK:
		if v.Unit == progress.None {
			return "done in " + fmtDuration(d)
		}
		s := "done, " + fmtAmount(v.Done, v.Unit) + " in " + fmtDuration(d)
		if d >= 500*time.Millisecond {
			s += ", " + fmtRate(progress.MeanRate(v), v.Unit)
		}
		return s
	case progress.Warning:
		return "done with a warning: " + clean(v.Warn)
	case progress.Failed:
		return "failed" + stoppedAt(v) + ": " + clean(v.Reason)
	case progress.Interrupted:
		return "interrupted" + stoppedAt(v)
	case progress.Skipped:
		return "skipped: " + clean(v.Reason)
	}
	return "ended"
}

// plainStart is the plain line for a phase that started.
func plainStart(v progress.View) string {
	if v.Total > 0 {
		return "start, " + fmtAmount(v.Total, v.Unit)
	}
	return "start"
}

// plainBeat is the plain heartbeat line for a running phase.
func plainBeat(v progress.View, rate float64, now time.Time) string {
	if v.Total > 0 {
		s := itoa(percent(v.Done, v.Total, true)) + "%, " + strings.Replace(fmtPair(v.Done, v.Total, v.Unit), "/", " of ", 1) + ", " + fmtRate(rate, v.Unit)
		if d, ok := progress.ETA(v, rate, now); ok {
			s += ", eta " + fmtETA(d)
		}
		return s
	}
	running := "running for " + fmtDuration(now.Sub(v.Started))
	if v.Unit == progress.None {
		return running
	}
	return fmtAmount(v.Done, v.Unit) + ", " + fmtRate(rate, v.Unit) + ", " + running
}

// footerFields are the command's totals: each tally, "deduplicated" when
// the command read bytes and only some of them were new, and "compressed"
// when it wrote new bytes.
func footerFields(s progress.Snapshot) []string {
	var fields []string
	var read, fresh, written int64
	var haveRead, haveNew bool
	for _, m := range s.Tallies {
		fields = append(fields, m.Name+" "+fmtAmount(m.Value, m.Unit))
		switch {
		case m.Name == "read" && m.Unit == progress.Bytes:
			read, haveRead = m.Value, true
		case m.Name == "new" && m.Unit == progress.Bytes:
			fresh, haveNew = m.Value, true
		case m.Name == "written" && m.Unit == progress.Bytes:
			written = m.Value
		}
	}
	ok := s.Outcome == progress.OK || s.Outcome == progress.Warning
	if ok && haveRead && haveNew && read > 0 && fresh <= read {
		dup := read - fresh
		fields = append(fields, "deduplicated "+fmtBytes(dup)+" ("+itoa(int(dup*100/read))+"%)")
	}
	if ok && fresh > 0 && written > 0 {
		fields = append(fields, "compressed "+Ratio(fresh, written))
	}
	return fields
}

// outcomeWords opens the footer: "done in", "failed after"...
func outcomeWords(o progress.State) string {
	switch o {
	case progress.Warning:
		return "done with warnings in"
	case progress.Failed:
		return "failed after"
	case progress.Interrupted:
		return "interrupted after"
	}
	return "done in"
}

// firstBytes is the first tally counted in bytes, for the average rate.
func firstBytes(s progress.Snapshot) (int64, bool) {
	for _, m := range s.Tallies {
		if m.Unit == progress.Bytes {
			return m.Value, true
		}
	}
	return 0, false
}
