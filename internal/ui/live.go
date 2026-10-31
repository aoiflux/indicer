package ui

import (
	"slices"
	"strings"
	"time"

	"indicer/internal/progress"

	"github.com/charmbracelet/x/ansi"
)

// The live display is one overall row for the command as a whole and,
// under it, a row for each step: faint, indented, without bars, as docker
// draws its layers. liveView lays out one frame from a snapshot's estimate.
// It keeps no state, so tests draw frames without a runner.
type liveView struct {
	p     Painter
	g     Glyphs
	frame int
	pulse map[uint64]int // OK marks shown in highlight until this frame
	rate  func(id uint64) float64
	now   time.Time
}

// liveLines is the most lines the live region takes.
const liveLines = 7

// lines is a live frame: the overall row, then the detail rows, in at most
// min(7, height-2) lines; with under 4 rows of terminal, the overall row
// alone.
func (lv liveView) lines(e progress.Estimate, overall string, width, height int) []string {
	if height < 4 {
		return []string{overall}
	}
	return append([]string{overall}, lv.details(e, width, min(liveLines, height-2)-1)...)
}

// overall is the row for the command as a whole: a bar and its percentage
// for measured work, a sweep for work of unknown size, then what the focus
// step is doing, its rate and the time left.
func (lv liveView) overall(e progress.Estimate, width int) string {
	p, g := lv.p, lv.g
	f := e.Focus
	spin := p.Paint(Highlight, g.Spinner[lv.frame%len(g.Spinner)])
	label := clean(StepLabel(f.Name, f.State))
	rate := part{p.Paint(Muted, fmtRate(e.Rate, f.Unit)), 3}
	switch e.Mode {
	case progress.ModeNone:
		return ansi.Truncate(spin+" working "+p.Paint(Muted, fmtDuration(e.Elapsed)), width, "")
	case progress.ModeActivity:
		parts := []part{{}, {label, 7}}
		if f.Unit != progress.None {
			parts = append(parts, part{fmtAmount(f.Done, f.Unit), 5}, rate)
		}
		parts = append(parts, part{p.Paint(Muted, fmtDuration(e.Elapsed)), 4})
		return spin + " " + fit(parts, 0, func(w int) string { return sweep(p, g, w, lv.frame) }, width-2)
	}
	parts := []part{{}, {p.Bold(itoa(pct(e.Fraction)) + "%"), keepAlways}}
	switch e.Mode {
	case progress.ModeWork:
		if e.Steps > 1 {
			parts = append(parts, part{p.Paint(Muted, "step") + " " + itoa(e.Step) + "/" + itoa(e.Steps), 1})
		}
		switch {
		case e.InWork:
			if e.Steps <= 1 {
				parts = append(parts, part{label, 1})
			}
			parts = append(parts, part{fmtPair(f.Done, f.Total, f.Unit), 2}, rate)
		case f.State == progress.Running:
			// Something else runs while the next work step waits.
			parts = append(parts, part{label, 6})
			if f.Unit != progress.None {
				parts = append(parts, part{fmtAmount(f.Done, f.Unit), 5}, rate)
			}
		}
		if e.HasETA && e.Whole {
			parts = append(parts, part{lv.eta(e), 4})
		}
	case progress.ModeItems:
		parts = append(parts, part{fmtPair(e.Done, e.Total, e.Unit), 2})
		if f.State == progress.Running && f.Unit != progress.None {
			parts = append(parts, rate)
		}
		if e.HasETA {
			parts = append(parts, part{lv.eta(e), 4})
		}
	case progress.ModeStep:
		parts = append(parts, part{label, 7}, part{fmtPair(f.Done, f.Total, f.Unit), 2}, rate)
		if e.HasETA {
			parts = append(parts, part{lv.eta(e), 4})
		}
	}
	return spin + " " + fit(parts, 0, func(w int) string { return bar(p, g, int64(e.Fraction*1e6), 1e6, w, Forensics) }, width-2)
}

// eta is "ETA 11s", or "ETA ~11s" when other running work may move it.
func (lv liveView) eta(e progress.Estimate) string {
	s := fmtETA(e.ETA)
	if e.Approx {
		s = "~" + s
	}
	return lv.p.Paint(Muted, "ETA") + " " + s
}

// pct is a fraction as a whole percentage, rounded down.
func pct(f float64) int { return int(f*100 + 1e-9) }

// detail is one row under the overall row; rank says which rows go first
// when there is no room.
type detail struct {
	s    string
	rank int
}

// rank orders rows for dropping: finished rows go before the rest.
func rank(s progress.State) int {
	if s == progress.OK || s == progress.Skipped {
		return 1
	}
	return 3
}

// details are the rows under the overall row, at most budget of them: one
// for each step the estimate measured (for a folder, the running item and
// its steps), the pending ones folded into one "next:" row. A scope of one
// step, which the overall row already names, has none.
func (lv liveView) details(e progress.Estimate, width, budget int) []string {
	scope := e.Scope
	if budget <= 0 || (len(scope) == 1 && scope[0].ID == e.Focus.ID && e.Mode != progress.ModeItems) {
		return nil
	}
	nameW := labelWidth(scope)
	var rows []detail
	var pending []string
	for i := range scope {
		v := &scope[i]
		switch {
		case e.Mode == progress.ModeItems && e.Group != nil && v.ID == e.Group.ID:
			if e.Item != nil {
				rows = append(rows, lv.itemRows(e, width)...)
			}
		case v.State == progress.Pending:
			pending = append(pending, clean(v.Name))
		default:
			rows = append(rows, detail{lv.row(*v, 0, nameW, v.ID == e.Focus.ID, e, true, width), rank(v.State)})
		}
	}
	if len(pending) > 0 {
		rows = append(rows, detail{lv.next(pending, 0, width), 2})
	}
	return lv.trim(rows, budget)
}

// itemRows are a folder's running item and, one level in, its steps.
func (lv liveView) itemRows(e progress.Estimate, width int) []detail {
	it := e.Item
	head := "  " + lv.mark(*it) + " "
	share := ""
	if e.ItemFraction > 0 {
		share = "  " + lv.p.Faint(itoa(min(pct(e.ItemFraction), 99))+"%")
	}
	name := cutLeft(clean(it.Name), width-ansi.StringWidth(head)-ansi.StringWidth(share), lv.g.Ellipsis)
	rows := []detail{{ansi.Truncate(head+lv.p.Faint(name)+share, width, ""), 3}}
	nameW := labelWidth(it.Kids)
	var pending []string
	for _, k := range it.Kids {
		if k.State == progress.Pending {
			pending = append(pending, clean(k.Name))
			continue
		}
		rows = append(rows, detail{lv.row(k, 1, nameW, k.ID == e.Focus.ID, e, true, width), rank(k.State)})
	}
	if len(pending) > 0 {
		rows = append(rows, detail{lv.next(pending, 1, width), 2})
	}
	return rows
}

// trim keeps at most budget rows. The lowest rank goes first, oldest
// first; when two or more go, a row says how many.
func (lv liveView) trim(rows []detail, budget int) []string {
	drop := len(rows) - budget
	if drop > 1 {
		drop++ // room for the "more" row
	}
	dropped := 0
	for r := 0; drop > 0 && r <= 3; r++ {
		for i := 0; i < len(rows) && drop > 0; {
			if rows[i].rank == r {
				rows = slices.Delete(rows, i, i+1)
				drop--
				dropped++
				continue
			}
			i++
		}
	}
	out := make([]string, 0, len(rows)+1)
	for _, r := range rows {
		out = append(out, r.s)
	}
	if dropped > 1 {
		out = append(out, "  "+lv.p.Faint(lv.g.Ellipsis+" "+itoa(dropped)+" more"))
	}
	return out
}

// next is pending steps folded into one row.
func (lv liveView) next(names []string, depth, width int) string {
	g := lv.g
	s := strings.Repeat("  ", depth+1) + lv.p.Faint(g.Pending+" next: "+strings.Join(names, " "+g.Sep+" "))
	return ansi.Truncate(s, width, "")
}

// final is the frame left in the scrollback when the command ends: each
// top-level step by its name, not dimmed, and the steps that never ran on
// one line.
func (lv liveView) final(snap progress.Snapshot, width int) []string {
	lv.pulse = nil
	var shown []progress.View
	for _, v := range snap.Phases {
		if v.State != progress.Skipped || v.Reason != "not run" {
			shown = append(shown, v)
		}
	}
	nameW := labelWidth(shown)
	var lines, notRun []string
	for _, v := range snap.Phases {
		if v.State == progress.Skipped && v.Reason == "not run" {
			notRun = append(notRun, clean(v.Name))
			continue
		}
		lines = append(lines, lv.row(v, 0, nameW, false, progress.Estimate{}, false, width))
	}
	if len(notRun) > 0 {
		s := "  " + lv.p.Paint(Muted, lv.g.Skipped+" not run: "+strings.Join(notRun, ", "))
		lines = append(lines, ansi.Truncate(s, width, ""))
	}
	return lines
}

// itemDone is the line a folder item leaves above the live region when it
// ends.
func (lv liveView) itemDone(v progress.View, depth, width int) string {
	lv.pulse = nil
	return lv.row(v, depth, 0, false, progress.Estimate{}, false, width)
}

// labelWidth is the width labels are padded to, so columns stay put as
// steps start and end: the widest a step of vs reads, at most 20 columns.
func labelWidth(vs []progress.View) int {
	w := 0
	for _, v := range vs {
		if v.State != progress.Pending {
			w = max(w, ansi.StringWidth(clean(StepLabel(v.Name, progress.Running))), ansi.StringWidth(clean(v.Name)))
		}
	}
	return min(w, 20)
}

// row is one step: its mark and label, then what it has done, joined by
// " · ". A live row (faint) dims all but its mark, unless it is a warning
// or a failure; a final row, left in the scrollback, is not dimmed.
func (lv liveView) row(v progress.View, depth, nameW int, focus bool, e progress.Estimate, faint bool, width int) string {
	p, g := lv.p, lv.g
	if v.State == progress.Warning || v.State == progress.Failed || v.State == progress.Interrupted {
		faint = false
	}
	dim := func(s string) string {
		if faint {
			return p.Faint(s)
		}
		return s
	}
	mute := func(s string) string {
		if faint {
			return p.Faint(s)
		}
		return p.Paint(Muted, s)
	}
	name := clean(StepLabel(v.Name, v.State))
	if pad := nameW - ansi.StringWidth(name); pad > 0 {
		name += strings.Repeat(" ", pad)
	}
	parts := []part{{strings.Repeat("  ", depth+1) + lv.mark(v) + " " + dim(name), keepAlways}}
	flexAt, flexFn := -1, func(int) string { return "" }
	switch v.State {
	case progress.Running:
		switch {
		case v.Total > 0:
			parts = append(parts, part{dim(itoa(percent(v.Done, v.Total, true)) + "%"), 8})
			if focus && e.HasETA && !e.Whole {
				parts = append(parts, part{dim(fmtETA(e.ETA) + " left"), 7})
			}
		case v.Unit != progress.None:
			parts = append(parts, part{dim(fmtAmount(v.Done, v.Unit)), 5})
		default:
			parts = append(parts, part{mute(fmtDuration(lv.now.Sub(v.Started))), 4})
		}
		parts = append(parts, meterParts(v, dim)...)
		if !focus && v.Unit != progress.None && lv.rate != nil {
			parts = append(parts, part{mute(fmtRate(lv.rate(v.ID), v.Unit)), 0})
		}
		if v.Note != "" {
			parts = append(parts, part{dim(clean(v.Note)), 1})
		}
		if v.Item != "" {
			item := clean(v.Item)
			// The file goes before the note (a partition) when room is short.
			flexAt, flexFn = len(parts), func(room int) string { return dim(cutLeft(item, room, g.Ellipsis)) }
			parts = append(parts, part{item, 0})
		}
	case progress.OK, progress.Warning:
		d := v.Ended.Sub(v.Started)
		done := fmtDuration(d)
		if v.Unit != progress.None {
			done = fmtAmount(v.Done, v.Unit) + " in " + done
		}
		parts = append(parts, part{dim(done), 6})
		if v.Unit != progress.None && d >= 500*time.Millisecond {
			parts = append(parts, part{mute(fmtRate(progress.MeanRate(v), v.Unit)), 2})
		}
		parts = append(parts, meterParts(v, dim)...)
		if v.State == progress.Warning && v.Warn != "" {
			parts = append(parts, part{p.Strong(Warning, "warning:") + " " + clean(v.Warn), 3})
		}
	case progress.Failed:
		parts = append(parts, part{p.Strong(Error, "failed") + stoppedAt(v) + ": " + clean(v.Reason), 8})
	case progress.Interrupted:
		parts = append(parts, part{p.Strong(Warning, "interrupted") + stoppedAt(v), 8})
	case progress.Skipped:
		parts = append(parts, part{mute("skipped: " + clean(v.Reason)), 8})
	}
	return fitSep(parts, flexAt, flexFn, " "+mute(g.Sep)+" ", width)
}

// meterParts are a step's second counts ("new 1.80 GiB") and, for bytes of
// which some were stored already, how much ("31% dedup"). While the step
// runs these outlast its rate; once it ended, its rate outlasts them.
func meterParts(v progress.View, dim func(string) string) []part {
	newKeep, dedupKeep := 2, 3
	if v.State.Ended() {
		newKeep, dedupKeep = 1, 1
	}
	var parts []part
	for _, m := range v.Meters {
		keep := 1
		if m.Name == "new" {
			keep = newKeep
		}
		parts = append(parts, part{dim(m.Name + " " + fmtAmount(m.Value, m.Unit)), keep})
		if d, ok := dedupOf(v, m); ok && d > 0 {
			parts = append(parts, part{dim(itoa(d) + "% dedup"), dedupKeep})
		}
	}
	return parts
}

// dedupOf is how much of a byte step's input was stored already, once
// enough of it has been read to say: 1% of it, or 64 MiB. The staged
// pipeline counts new bytes well after it reads them.
func dedupOf(v progress.View, m progress.MeterView) (int, bool) {
	if v.Unit != progress.Bytes || m.Name != "new" || m.Unit != progress.Bytes || v.Done <= 0 || m.Value > v.Done {
		return 0, false
	}
	if v.Done < 64<<20 && (v.Total <= 0 || v.Done*100 < v.Total) {
		return 0, false
	}
	return int((v.Done - m.Value) * 100 / v.Done), true
}

// mark is a step's mark; one that just ended OK pulses in highlight.
func (lv liveView) mark(v progress.View) string {
	if v.State == progress.OK && lv.frame < lv.pulse[v.ID] {
		return lv.p.Strong(Highlight, lv.g.OK)
	}
	return stateMark(lv.p, lv.g, v.State, lv.frame)
}

// stateMark is a state's mark, coloured; a running step's spins.
func stateMark(p Painter, g Glyphs, s progress.State, frame int) string {
	switch s {
	case progress.Pending:
		return p.Paint(Muted, g.Pending)
	case progress.Running:
		return p.Paint(Highlight, g.Spinner[frame%len(g.Spinner)])
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

// sweep is the bar of work of unknown size: a segment a fifth of the bar
// long, at least 3 cells, that moves one cell a frame and turns at the ends.
func sweep(p Painter, g Glyphs, width, frame int) string {
	if width <= 0 {
		return ""
	}
	seg := max(3, width/5)
	if seg >= width {
		return p.Paint(Forensics, strings.Repeat(g.BarFull, width))
	}
	span := width - seg
	at := frame % (2 * span)
	if at > span {
		at = 2*span - at
	}
	return p.Faint(strings.Repeat(g.BarEmpty, at)) + p.Paint(Forensics, strings.Repeat(g.BarFull, seg)) +
		p.Faint(strings.Repeat(g.BarEmpty, span-at))
}

// cutLeft shortens s to at most w columns from the left, so its end (a
// file's name) stays: "…\config\SOFTWARE".
func cutLeft(s string, w int, ellipsis string) string {
	sw := ansi.StringWidth(s)
	if sw <= w {
		return s
	}
	ew := ansi.StringWidth(ellipsis)
	if w <= ew {
		return ""
	}
	return ansi.TruncateLeft(s, sw-w+ew, ellipsis)
}

// fitSep joins parts into at most width columns: the first (the head) to
// the rest by two spaces, the rest by sep. Parts with the lowest keep go
// first. The part at flexAt (a path, drawn by flexFn) takes the room left,
// but needs 8 columns.
func fitSep(parts []part, flexAt int, flexFn func(room int) string, sep string, width int) string {
	sepW := ansi.StringWidth(sep)
	for {
		fixed, flex, n := 0, -1, 0
		for i, pt := range parts {
			if pt.s == "" {
				continue
			}
			if i > 0 {
				if n == 0 {
					fixed += 2
				} else {
					fixed += sepW
				}
				n++
			}
			if i == flexAt {
				flex = i
				continue
			}
			fixed += ansi.StringWidth(pt.s)
		}
		need := fixed
		if flex >= 0 {
			need += min(ansi.StringWidth(parts[flex].s), 8)
		}
		if need > width && dropLowest(parts, -1) {
			continue
		}
		var b strings.Builder
		n = 0
		for i, pt := range parts {
			if pt.s == "" {
				continue
			}
			if i > 0 {
				if n == 0 {
					b.WriteString("  ")
				} else {
					b.WriteString(sep)
				}
				n++
			}
			if i == flex {
				b.WriteString(flexFn(width - fixed))
				continue
			}
			b.WriteString(pt.s)
		}
		return ansi.Truncate(b.String(), width, "")
	}
}
