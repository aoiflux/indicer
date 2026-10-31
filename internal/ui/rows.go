package ui

import (
	"math"
	"strings"
	"time"

	"indicer/internal/progress"
	"indicer/internal/termsafe"

	"github.com/charmbracelet/x/ansi"
)

// rateEst estimates a phase's rate: the mean rate for its first two
// seconds, then an exponentially weighted average over about five seconds.
type rateEst struct {
	lastDone int64
	lastT    time.Time
	rate     float64
}

const (
	rateSeed = 2 * time.Second
	rateTau  = 5.0 // seconds
)

func (e *rateEst) sample(v progress.View, now time.Time) float64 {
	if e.lastT.IsZero() {
		e.lastT, e.lastDone = v.Started, 0
	}
	dt := now.Sub(e.lastT).Seconds()
	if dt <= 0 {
		return e.rate
	}
	if now.Sub(v.Started) < rateSeed {
		e.rate = float64(v.Done) / now.Sub(v.Started).Seconds()
	} else {
		inst := float64(v.Done-e.lastDone) / dt
		e.rate += (1 - math.Exp(-dt/rateTau)) * (inst - e.rate)
	}
	e.lastT, e.lastDone = now, v.Done
	return e.rate
}

// eta is the remaining time, when it means something: a known total, two
// seconds of history, at least 1% done and a rate above zero.
func eta(v progress.View, rate float64, now time.Time) (time.Duration, bool) {
	if v.State != progress.Running || v.Total <= 0 || rate <= 0 || now.Sub(v.Started) < rateSeed || v.Done*100 < v.Total {
		return 0, false
	}
	return time.Duration(float64(v.Total-v.Done) / rate * float64(time.Second)), true
}

// meanRate is a finished phase's average rate.
func meanRate(v progress.View) float64 {
	d := v.Ended.Sub(v.Started).Seconds()
	if d <= 0 {
		return 0
	}
	return float64(v.Done) / d
}

// clean sanitises text from evidence (names, notes, errors) for one line.
func clean(s string) string { return termsafe.Line(s) }

// mark is the state's glyph, coloured.
func (rn *runner) mark(s progress.State) string {
	g, p := rn.g, rn.p
	switch s {
	case progress.Pending:
		return p.Paint(Muted, g.Pending)
	case progress.Running:
		return p.Paint(Highlight, g.Spinner[rn.frame%len(g.Spinner)])
	case progress.OK:
		return p.Strong(Success, g.OK)
	case progress.Warning:
		return p.Strong(Warning, g.Warn)
	case progress.Failed:
		return p.Strong(Error, g.Fail)
	case progress.Interrupted:
		return p.Strong(Warning, g.Interrupted)
	}
	return p.Paint(Muted, g.Skipped)
}

// part is one field of a row; rows drop fields with the lowest keep first
// until they fit the terminal.
type part struct {
	s    string
	keep int // 0 is dropped first; never dropped above keepAlways
}

const keepAlways = 9

// fit joins parts with two spaces into at most width columns. The bar at
// index barAt (drawn by barFn, 10 to 40 cells) takes the room left; when
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
			barW = min(width-others, 40)
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

// row renders one phase for the live display, at most width columns.
func (rn *runner) row(v progress.View, depth, nameW int, now time.Time, width int) string {
	p := rn.p
	indent := strings.Repeat("  ", depth+1)
	name := clean(v.Name)
	if pad := nameW - ansi.StringWidth(name); pad > 0 {
		name += strings.Repeat(" ", pad)
	}
	head := indent + rn.mark(v.State) + " " + name
	parts := []part{{head, keepAlways}}
	barAt := -1
	var barFn func(int) string

	switch v.State {
	case progress.Pending:
		return ansi.Truncate(indent+rn.mark(v.State)+" "+p.Paint(Muted, clean(v.Name)), width, "")
	case progress.Running:
		rate := rn.rates[v.ID].rate
		if v.Total > 0 {
			barAt = len(parts)
			parts = append(parts, part{})
			barFn = func(w int) string { return bar(p, rn.g, v.Done, v.Total, w, Forensics) }
			parts = append(parts,
				part{p.Bold(itoa(percent(v.Done, v.Total, true)) + "%"), keepAlways},
				part{fmtPair(v.Done, v.Total, v.Unit), 2},
				part{p.Paint(Muted, fmtRate(rate, v.Unit)), 3})
		} else if v.Unit != progress.None {
			count := fmtAmount(v.Done, v.Unit)
			if v.Note != "" {
				count += " " + p.Paint(Muted, rn.g.Sep) + " " + clean(v.Note)
			}
			parts = append(parts, part{count, 5}, part{p.Paint(Muted, fmtRate(rate, v.Unit)), 3})
		} else if v.Note != "" {
			parts = append(parts, part{clean(v.Note), 2})
		}
		for _, m := range v.Meters {
			parts = append(parts, part{p.Paint(Muted, m.Name) + " " + fmtAmount(m.Value, m.Unit), 1})
		}
		if d, ok := eta(v, rate, now); ok {
			parts = append(parts, part{p.Paint(Muted, "ETA") + " " + fmtETA(d), 4})
		} else if v.Total <= 0 {
			parts = append(parts, part{p.Paint(Muted, fmtDuration(now.Sub(v.Started))), 4})
		}
	case progress.OK, progress.Warning:
		if v.Unit != progress.None {
			parts = append(parts, part{fmtAmount(v.Done, v.Unit), 5})
		}
		parts = append(parts, part{p.Paint(Muted, fmtDuration(v.Ended.Sub(v.Started))), 6})
		if v.Unit != progress.None && v.Ended.Sub(v.Started) >= 500*time.Millisecond {
			parts = append(parts, part{p.Paint(Muted, fmtRate(meanRate(v), v.Unit)), 2})
		}
		for _, m := range v.Meters {
			parts = append(parts, part{p.Paint(Muted, m.Name) + " " + fmtAmount(m.Value, m.Unit), 1})
		}
		if v.State == progress.Warning && v.Warn != "" {
			parts = append(parts, part{p.Strong(Warning, "warning:") + " " + clean(v.Warn), 3})
		}
	case progress.Failed:
		parts = append(parts, part{p.Strong(Error, "failed") + stoppedAt(v) + ": " + clean(v.Reason), 8})
	case progress.Interrupted:
		parts = append(parts, part{p.Strong(Warning, "interrupted") + stoppedAt(v), 8})
	case progress.Skipped:
		parts = append(parts, part{p.Paint(Muted, "skipped: "+clean(v.Reason)), 8})
	}
	return fit(parts, barAt, barFn, width)
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
			s += ", " + fmtRate(meanRate(v), v.Unit)
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
		if d, ok := eta(v, rate, now); ok {
			s += ", eta " + fmtETA(d)
		}
		return s
	}
	if v.Unit == progress.None {
		return "running for " + fmtDuration(now.Sub(v.Started))
	}
	return fmtAmount(v.Done, v.Unit) + ", " + fmtRate(rate, v.Unit)
}

// footerFields are the command's totals: each tally, and "deduplicated"
// when the command read bytes and only some of them were new.
func footerFields(s progress.Snapshot) []string {
	var fields []string
	var read, fresh int64
	var haveRead, haveNew bool
	for _, m := range s.Tallies {
		fields = append(fields, m.Name+" "+fmtAmount(m.Value, m.Unit))
		switch {
		case m.Name == "read" && m.Unit == progress.Bytes:
			read, haveRead = m.Value, true
		case m.Name == "new" && m.Unit == progress.Bytes:
			fresh, haveNew = m.Value, true
		}
	}
	ok := s.Outcome == progress.OK || s.Outcome == progress.Warning
	if ok && haveRead && haveNew && read > 0 && fresh <= read {
		dup := read - fresh
		fields = append(fields, "deduplicated "+fmtBytes(dup)+" ("+itoa(int(dup*100/read))+"%)")
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
