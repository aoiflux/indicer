package ui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/internal/progress"
)

type renderMode uint8

const (
	renderOff renderMode = iota
	renderLive
	renderPlain
)

// runConfig is how a progress display runs; tests build their own.
type runConfig struct {
	mode      renderMode
	now       func() time.Time
	tick      time.Duration // sampling interval
	heartbeat time.Duration // plain progress line interval
	hold      time.Duration // a partial line held this long is a prompt
	size      func() (width, height int)
	painter   Painter   // for the progress stream
	progress  io.Writer // where frames and lines go: the raw stderr
	shared    bool      // stdout is the same terminal as stderr
	// taskbar shows progress on the terminal's tab once a command has run
	// for taskbarDelay, so quick commands never flicker it.
	taskbar      bool
	taskbarDelay time.Duration
	wrap         func(func(out, err io.Writer) (io.Writer, io.Writer)) func()
	manual       bool // tests call step themselves
}

func defaultRunConfig() runConfig {
	m := Current()
	cfg := runConfig{
		mode: renderPlain, now: time.Now, tick: 250 * time.Millisecond,
		heartbeat: 15 * time.Second, hold: 250 * time.Millisecond,
		size: termSize, painter: For(console.Progress()), progress: console.Progress(),
		wrap: console.Wrap, taskbarDelay: time.Second,
	}
	switch {
	case console.Progress() == io.Discard:
		cfg.mode = renderOff
	case m.Err.Motion:
		cfg.mode, cfg.tick = renderLive, 100*time.Millisecond
		cfg.shared = m.Out.TTY && console.Detect().Shared
		cfg.taskbar = m.Taskbar
	}
	return cfg
}

// Run runs fn with a progress reporter in its context and shows what fn
// reports on stderr: a live display on a terminal, plain lines elsewhere,
// nothing when output is silenced. Once fn returns it ends with one footer
// line and returns fn's error. A panic in fn still leaves the terminal as
// it was, and goes on.
func Run(ctx context.Context, fn func(context.Context) error) error {
	return run(ctx, defaultRunConfig(), fn)
}

var errPanicked = errors.New("stopped by an internal error")

func run(ctx context.Context, cfg runConfig, fn func(context.Context) error) (err error) {
	if cfg.mode == renderOff {
		return fn(progress.Without(ctx))
	}
	rep := progress.New(ctx, cfg.now)
	rn := newRunner(cfg, rep)
	rn.start()
	finished := false
	defer func() {
		if !finished {
			rn.stop(errPanicked)
		}
	}()
	err = fn(progress.With(ctx, rep))
	finished = true
	rn.stop(err)
	return err
}

// runner draws one command's progress.
type runner struct {
	cfg runConfig
	rep *progress.Reporter
	p   Painter
	g   Glyphs

	mu        sync.Mutex
	drawn     int      // live: rows of the region on screen
	lastLines []string // live: the region as last drawn
	hidden    bool     // the cursor is hidden by us
	paused    *coord   // a prompt holds the screen: no redraws
	coords    []*coord
	queue     []string // plain: lines waiting for a held partial line
	lastSeq   uint64
	frame     int
	track     *progress.Tracker
	est       progress.Estimate // the command as a whole, as of the last sample
	pulse     map[uint64]int    // live: steps that ended OK, and the frame their pulse ends
	lastOver  string            // live: the last overall row with something running
	noneSince time.Time         // live: since when nothing has run
	tb        taskbar           // live: what the terminal's tab shows
	lastBeat  time.Time
	stopped   bool

	quit    chan struct{}
	exited  chan struct{}
	restore func()
	unhook  func()
}

func newRunner(cfg runConfig, rep *progress.Reporter) *runner {
	return &runner{
		cfg: cfg, rep: rep, p: cfg.painter, g: cfg.painter.Glyphs(),
		track: progress.NewTracker(), pulse: map[uint64]int{}, lastBeat: cfg.now(),
		quit: make(chan struct{}), exited: make(chan struct{}),
	}
}

func (rn *runner) start() {
	rn.restore = rn.cfg.wrap(func(out, err io.Writer) (io.Writer, io.Writer) {
		e := rn.coordinate(err)
		if rn.cfg.mode == renderLive && rn.cfg.shared {
			out = rn.coordinate(out)
		}
		return out, e
	})
	rn.unhook = lifecycle.OnForceExit(rn.emergency)
	if rn.cfg.manual {
		close(rn.exited)
		return
	}
	go func() {
		defer close(rn.exited)
		t := time.NewTicker(rn.cfg.tick)
		defer t.Stop()
		for {
			select {
			case <-rn.quit:
				return
			case <-t.C:
				rn.step()
			}
		}
	}()
}

// step samples the reporter and draws.
func (rn *runner) step() {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	if rn.stopped {
		return
	}
	now := rn.cfg.now()
	snap := rn.rep.Snapshot(rn.lastSeq)
	rn.lastSeq = snap.Seq
	rn.est = rn.track.Update(snap, now)
	rn.checkHolds(now)
	switch rn.cfg.mode {
	case renderLive:
		rn.liveEvents(snap)
		if rn.paused == nil {
			rn.drawLocked(rn.frameLines(snap, now, false))
		}
		rn.updateTaskbar(snap, now)
		rn.frame++
	case renderPlain:
		rn.plainEvents(snap)
		if now.Sub(rn.lastBeat) >= beatEvery(now.Sub(snap.Start), rn.cfg.heartbeat) {
			rn.lastBeat = now
			rn.plainBeatLine(now)
		}
		rn.flushQueue()
	}
	rn.rep.Prune()
}

// stop ends the display: the last frame (or the last plain lines), the
// footer, held partial lines, the cursor, and the original sinks.
func (rn *runner) stop(err error) {
	close(rn.quit)
	select {
	case <-rn.exited:
	case <-time.After(2 * time.Second):
		// A stalled terminal must not hang the exit.
	}
	rn.rep.Finish(err)
	rn.mu.Lock()
	defer rn.mu.Unlock()
	now := rn.cfg.now()
	snap := rn.rep.Snapshot(rn.lastSeq)
	rn.lastSeq = snap.Seq
	rn.est = rn.track.Update(snap, now)
	switch rn.cfg.mode {
	case renderLive:
		rn.paused = nil
		rn.liveEvents(snap)
		rn.drawLocked(rn.frameLines(snap, now, true))
		rn.drawn = 0
		if len(snap.Phases) > 0 {
			rn.write(rn.liveFooter(snap, now))
		}
	case renderPlain:
		rn.plainEvents(snap)
		if len(snap.Phases) > 0 {
			rn.queue = append(rn.queue, rn.plainFooter(snap, now)...)
		}
	}
	for _, c := range rn.coords {
		if len(c.buf) > 0 {
			_, _ = c.raw.Write(c.buf)
			c.buf = nil
		}
	}
	for _, l := range rn.queue {
		rn.write(l)
	}
	rn.queue = nil
	rn.clearTaskbar()
	rn.showCursor()
	rn.stopped = true
	rn.restore()
	rn.unhook()
}

// emergency runs when a second signal ends the process at once: it leaves
// the terminal usable and says what happened.
func (rn *runner) emergency() {
	if !rn.mu.TryLock() {
		clear := ""
		if rn.cfg.taskbar {
			clear = osc94(tbClear, 0)
		}
		_, _ = rn.cfg.progress.Write([]byte("\r\n" + clear + "\x1b[?25h"))
		return
	}
	defer rn.mu.Unlock()
	if rn.stopped {
		return
	}
	const msg = "stopped at once; the next command that opens the database for writing recovers what was left"
	if rn.cfg.mode == renderLive {
		snap := rn.rep.Snapshot(rn.lastSeq)
		rn.interruptAll(snap.Phases)
		rn.drawLocked(rn.frameLines(snap, rn.cfg.now(), true))
		rn.drawn = 0
		width, _ := rn.cfg.size()
		rn.write(wrapLine(rn.p.Strong(Warning, rn.g.Interrupted)+" "+msg, width-1, "  ") + "\n")
	} else {
		rn.write("dues: " + msg + "\n")
	}
	rn.clearTaskbar()
	rn.showCursor()
	rn.stopped = true
}

// updateTaskbar shows the command's progress on the terminal's tab: the
// percentage of measured work, a busy state for work of unknown size, a
// paused state while a prompt waits or after an interrupt, an error state
// after a failure. Between two steps it keeps what it showed.
func (rn *runner) updateTaskbar(snap progress.Snapshot, now time.Time) {
	if !rn.cfg.taskbar || now.Sub(snap.Start) < rn.cfg.taskbarDelay {
		return
	}
	st := tbIndeterminate
	switch {
	case rn.paused != nil:
		st = tbPaused
	case anyTop(snap.Phases, progress.Failed):
		st = tbError
	case anyTop(snap.Phases, progress.Interrupted):
		st = tbPaused
	case rn.est.Mode == progress.ModeWork, rn.est.Mode == progress.ModeItems, rn.est.Mode == progress.ModeStep:
		st = tbNormal
	case rn.est.Mode == progress.ModeNone && !rn.noneSince.IsZero() && now.Sub(rn.noneSince) < time.Second:
		return
	}
	if s := rn.tb.next(st, pct(rn.est.Fraction), now); s != "" {
		rn.write(s)
	}
}

// clearTaskbar takes the progress off the terminal's tab.
func (rn *runner) clearTaskbar() {
	if s := rn.tb.clear(); s != "" {
		rn.write(s)
	}
}

func anyTop(vs []progress.View, s progress.State) bool {
	for _, v := range vs {
		if v.State == s {
			return true
		}
	}
	return false
}

func (rn *runner) interruptAll(vs []progress.View) {
	for i := range vs {
		if vs[i].State == progress.Running {
			vs[i].State = progress.Interrupted
		}
		rn.interruptAll(vs[i].Kids)
	}
}

func (rn *runner) write(s string) { _, _ = rn.cfg.progress.Write([]byte(s)) }

func (rn *runner) showCursor() {
	if rn.hidden {
		rn.write("\x1b[?25h")
		rn.hidden = false
	}
}

// ---- live region ------------------------------------------------------

// drawLocked replaces the region with lines.
func (rn *runner) drawLocked(lines []string) {
	var b strings.Builder
	if !rn.hidden && len(lines) > 0 {
		b.WriteString("\x1b[?25l")
		rn.hidden = true
	}
	if rn.drawn > 0 {
		b.WriteString("\r\x1b[" + strconv.Itoa(rn.drawn) + "A")
	}
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\x1b[K\n")
	}
	if rn.drawn > len(lines) {
		b.WriteString("\x1b[J")
	}
	rn.drawn = len(lines)
	rn.lastLines = lines
	if b.Len() > 0 {
		rn.write(b.String())
	}
}

// eraseLocked removes the region from the screen.
func (rn *runner) eraseLocked() {
	if rn.drawn > 0 {
		rn.write("\r\x1b[" + strconv.Itoa(rn.drawn) + "A\x1b[J")
		rn.drawn = 0
	}
}

// aboveLocked prints s above the region: erase, print, draw again.
func (rn *runner) aboveLocked(w io.Writer, s []byte) {
	if rn.cfg.mode != renderLive {
		_, _ = w.Write(s)
		return
	}
	lines := rn.lastLines
	rn.eraseLocked()
	_, _ = w.Write(s)
	if rn.paused == nil && lines != nil {
		rn.drawLocked(lines)
	}
}

// view is the live layout for one frame.
func (rn *runner) view(now time.Time) liveView {
	return liveView{p: rn.p, g: rn.g, frame: rn.frame, pulse: rn.pulse, rate: rn.track.Rate, now: now}
}

// frameLines lays out the region: the overall row and the steps under it,
// or, once the command ends, the top-level steps alone. Between two steps,
// when nothing runs, the last overall row stays up to a second.
func (rn *runner) frameLines(snap progress.Snapshot, now time.Time, final bool) []string {
	width, height := rn.cfg.size()
	width = max(width-1, 20)
	lv := rn.view(now)
	if final {
		return lv.final(snap, width)
	}
	if len(snap.Phases) == 0 {
		return nil
	}
	over := lv.overall(rn.est, width)
	if rn.est.Mode == progress.ModeNone && rn.lastOver != "" {
		if rn.noneSince.IsZero() {
			rn.noneSince = now
		}
		if now.Sub(rn.noneSince) < time.Second {
			over = rn.lastOver
		}
	} else {
		rn.noneSince, rn.lastOver = time.Time{}, over
	}
	return lv.lines(rn.est, over, width, height)
}

// liveEvents prints, above the region, the phases below the top level that
// ended: each item of a folder, and anything deeper that did not end OK.
// On a terminal stdout shares, the items' reports already say they were
// stored, so only items that did not end OK print. Steps that ended OK
// start their pulse.
func (rn *runner) liveEvents(snap progress.Snapshot) {
	width, _ := rn.cfg.size()
	lv := rn.view(rn.cfg.now())
	for _, e := range snap.Events {
		ok := e.Phase.State == progress.OK
		if e.Ended && ok {
			rn.pulse[e.Phase.ID] = rn.frame + 3
		}
		if !e.Ended || len(e.Path) == 0 || (ok && (len(e.Path) > 1 || rn.cfg.shared)) {
			continue
		}
		line := lv.itemDone(e.Phase, len(e.Path), max(width-1, 20))
		rn.aboveLocked(rn.cfg.progress, []byte(line+"\n"))
	}
	for id, until := range rn.pulse {
		if rn.frame >= until {
			delete(rn.pulse, id)
		}
	}
}

// liveFooter is the closing line (and hints) on a terminal.
func (rn *runner) liveFooter(snap progress.Snapshot, now time.Time) string {
	p, g := rn.p, rn.g
	elapsed := now.Sub(snap.Start)
	mark, role := g.OK, Success
	switch snap.Outcome {
	case progress.Warning:
		mark, role = g.Warn, Warning
	case progress.Failed:
		mark, role = g.Fail, Error
	case progress.Interrupted:
		mark, role = g.Interrupted, Warning
	}
	sep := " " + p.Paint(Muted, g.Sep) + " "
	parts := []string{p.Strong(role, mark) + " " + outcomeWords(snap.Outcome) + " " + fmtDuration(elapsed)}
	parts = append(parts, footerFields(snap)...)
	if n, ok := firstBytes(snap); ok && elapsed >= 500*time.Millisecond {
		parts = append(parts, fmtRate(float64(n)/elapsed.Seconds(), progress.Bytes)+" avg")
	}
	width, _ := rn.cfg.size()
	var b strings.Builder
	b.WriteString(wrapLine(strings.Join(parts, sep), width-1, "  ") + "\n")
	for _, h := range snap.Hints {
		b.WriteString(wrapLine("  "+p.Strong(Info, g.Note)+" "+clean(h), width-1, "    ") + "\n")
	}
	return b.String()
}

// ---- plain lines ------------------------------------------------------

// plainEvents writes start and end lines: start and end of every top-level
// phase, the end of each folder item, and deeper ends that were not OK. A
// phase that started and ended between two samples gets its end line only.
func (rn *runner) plainEvents(snap progress.Snapshot) {
	ended := map[uint64]bool{}
	for _, e := range snap.Events {
		if e.Ended {
			ended[e.Phase.ID] = true
		}
	}
	for _, e := range snap.Events {
		depth := len(e.Path)
		path := strings.Join(append(append([]string{}, e.Path...), e.Phase.Name), " / ")
		switch {
		case !e.Ended && depth == 0 && !ended[e.Phase.ID]:
			rn.plainLine(path, plainStart(e.Phase))
		case e.Ended && (depth <= 1 || e.Phase.State != progress.OK):
			rn.plainLine(path, plainEnd(e.Phase))
		}
	}
}

// beatEvery is how often plain output says how a command is going: every
// heartbeat at first, then a tenth of the time it has run, at most every
// five minutes, so a long run prints a few dozen lines and a CI log never
// goes silent for long.
func beatEvery(elapsed, heartbeat time.Duration) time.Duration {
	return min(max(elapsed/10, heartbeat), 5*time.Minute)
}

// plainBeatLine queues the heartbeat: one line on the focus step, how far
// the whole command is when it has several work steps or items, and what
// else runs beside it.
func (rn *runner) plainBeatLine(now time.Time) {
	e := rn.est
	f := e.Focus
	path := append([]string{}, e.FocusPath...)
	switch {
	case f.State == progress.Running:
		path = append(path, f.Name)
	case e.Mode == progress.ModeItems:
		// Between two items, or two steps of one: the count of items.
		f = *e.Group
		path = []string{f.Name}
	default:
		return
	}
	text := plainBeat(f, rn.track.Rate(f.ID), now)
	switch {
	case e.Mode == progress.ModeWork && e.Steps > 1:
		text += ", overall " + itoa(pct(e.Fraction)) + "% (step " + itoa(e.Step) + " of " + itoa(e.Steps) + ")"
	case e.Mode == progress.ModeItems && f.ID != e.Group.ID:
		text += ", overall " + itoa(pct(e.Fraction)) + "% (" + fmtCount(e.Done) + " of " + fmtAmount(e.Total, e.Unit) + ")"
	}
	for _, v := range e.Scope {
		if v.State != progress.Running || v.ID == f.ID || (e.Group != nil && v.ID == e.Group.ID) {
			continue
		}
		text += "; " + clean(v.Name) + " " + plainCount(v)
	}
	rn.plainLine(strings.Join(path, " / "), text)
}

// plainCount is how far a step beside the focus is, in a few words.
func plainCount(v progress.View) string {
	switch {
	case v.Total > 0:
		return itoa(percent(v.Done, v.Total, true)) + "%"
	case v.Unit != progress.None:
		return fmtAmount(v.Done, v.Unit)
	}
	return "running"
}

// plainLine queues "dues: <what>: <text>". Lines wait while another writer
// holds a partial line, so a prompt is never split.
func (rn *runner) plainLine(what, text string) {
	rn.queue = append(rn.queue, "dues: "+clean(what)+": "+text+"\n")
}

func (rn *runner) flushQueue() {
	for _, c := range rn.coords {
		if len(c.buf) > 0 || c.shown {
			return
		}
	}
	for _, l := range rn.queue {
		rn.write(l)
	}
	rn.queue = nil
}

// plainFooter is the closing line (and hints) in plain output.
func (rn *runner) plainFooter(snap progress.Snapshot, now time.Time) []string {
	fields := append([]string{outcomeWords(snap.Outcome) + " " + fmtDuration(now.Sub(snap.Start))}, footerFields(snap)...)
	lines := []string{"dues: " + strings.Join(fields, ", ") + "\n"}
	for _, h := range snap.Hints {
		lines = append(lines, "dues: hint: "+clean(h)+"\n")
	}
	return lines
}

// ---- coordinated writers ----------------------------------------------

// coord wraps stdout or stderr while the display runs. Complete lines go
// above the region; a partial line waits for its end, and one that waits
// longer than cfg.hold is a prompt: it is shown and redraws pause until
// its line ends.
type coord struct {
	rn    *runner
	raw   io.Writer
	buf   []byte
	since time.Time
	shown bool
}

func (rn *runner) coordinate(w io.Writer) io.Writer {
	c := &coord{rn: rn, raw: w}
	rn.coords = append(rn.coords, c)
	return c
}

func (c *coord) Write(p []byte) (int, error) {
	rn := c.rn
	rn.mu.Lock()
	defer rn.mu.Unlock()
	if rn.stopped || c.shown {
		n, err := c.raw.Write(p)
		if c.shown && bytes.IndexByte(p, '\n') >= 0 {
			c.shown = false
			if rn.paused == c {
				rn.paused = nil
			}
			if rn.cfg.mode == renderPlain && !rn.stopped {
				rn.flushQueue()
			}
		}
		return n, err
	}
	c.buf = append(c.buf, p...)
	i := bytes.LastIndexByte(c.buf, '\n')
	if i < 0 {
		if c.since.IsZero() {
			c.since = rn.cfg.now()
		}
		return len(p), nil
	}
	rn.aboveLocked(c.raw, c.buf[:i+1])
	c.buf = append([]byte(nil), c.buf[i+1:]...)
	c.since = time.Time{}
	if len(c.buf) > 0 {
		c.since = rn.cfg.now()
	}
	if rn.cfg.mode == renderPlain {
		rn.flushQueue()
	}
	return len(p), nil
}

// checkHolds shows partial lines held too long: they are prompts.
func (rn *runner) checkHolds(now time.Time) {
	for _, c := range rn.coords {
		if len(c.buf) == 0 || now.Sub(c.since) < rn.cfg.hold {
			continue
		}
		if rn.cfg.mode == renderLive {
			rn.drawn = 0 // the region stays on screen as it is
			rn.paused = c
			rn.showCursor()
		} else {
			for _, l := range rn.queue {
				rn.write(l)
			}
			rn.queue = nil
		}
		_, _ = c.raw.Write(c.buf)
		c.buf, c.since, c.shown = nil, time.Time{}, true
	}
}
