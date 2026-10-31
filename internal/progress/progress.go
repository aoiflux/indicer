// Package progress is how long operations report how far they are. It
// draws nothing: internal/ui renders what it collects.
//
// A Reporter travels in the context. Code that has no reporter in its
// context (the TUI, the server, the FFI library, the compression product,
// nested ingests) reports into nothing: every function and method here is
// safe on a nil reporter, phase or meter, and costs almost nothing then.
//
// Work is split into phases. A phase counts in one unit toward a total that
// may be unknown at first; several may run at once (the chunk pass and the
// index pass do). Hot paths only call Add, an atomic add.
package progress

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"indicer/internal/lifecycle"
)

// Unit is what a phase counts. Bytes are shown in IEC units; anything else
// is a count of things with that name.
type Unit string

const (
	None     Unit = ""
	Bytes    Unit = "bytes"
	Files    Unit = "files"
	Objects  Unit = "objects"
	Evidence Unit = "evidence"
	Records  Unit = "records"
)

// Unknown is the total of a phase whose size is not known yet.
const Unknown int64 = -1

// State is where a phase is.
type State uint8

const (
	Pending State = iota
	Running
	OK
	Warning
	Failed
	Interrupted
	Skipped
)

// Ended reports whether s is a final state.
func (s State) Ended() bool { return s >= OK }

type (
	reporterKey struct{}
	phaseKey    struct{}
)

// With returns ctx carrying r.
func With(ctx context.Context, r *Reporter) context.Context {
	return context.WithValue(ctx, reporterKey{}, r)
}

// From returns the reporter ctx carries, or nil.
func From(ctx context.Context) *Reporter {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(reporterKey{}).(*Reporter)
	return r
}

// Without returns ctx with no reporter, hiding any outer one: Quiet options
// use it, so nested work stays silent.
func Without(ctx context.Context) context.Context {
	if From(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, reporterKey{}, (*Reporter)(nil))
}

// Within returns ctx in which p is the current phase: phases started below
// are its children, and leaf helpers count into it.
func Within(ctx context.Context, p *Phase) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, phaseKey{}, p)
}

// Current is the phase ctx's work counts into, or nil. Only leaf helpers
// whose caller wrapped them with Within use it (HashContent, for one), so
// bytes never land on a phase that counts files.
func Current(ctx context.Context) *Phase {
	if ctx == nil {
		return nil
	}
	p, _ := ctx.Value(phaseKey{}).(*Phase)
	if p == nil || p.r != From(ctx) {
		return nil
	}
	return p
}

// CurrentIn is Current when the current phase counts in unit, otherwise
// nil, so a helper that counts bytes never adds them to a phase that counts
// files or evidence (one item of a folder store, say).
func CurrentIn(ctx context.Context, unit Unit) *Phase {
	p := Current(ctx)
	if p == nil {
		return nil
	}
	p.r.mu.Lock()
	defer p.r.mu.Unlock()
	if p.unit != unit {
		return nil
	}
	return p
}

// Plan lists phases that will run, in display order, so they show as
// pending before they start.
func Plan(ctx context.Context, names ...string) {
	if From(ctx) == nil {
		return
	}
	steps := make([]Step, len(names))
	for i, n := range names {
		steps[i] = Step{Name: n, Total: Unknown}
	}
	PlanSteps(ctx, steps...)
}

// Step is a phase a command expects to run: its name, and the unit and
// total it is expected to count (Total may be Unknown).
type Step struct {
	Name  string
	Unit  Unit
	Total int64
}

// PlanSteps is Plan with the work each step expects, so the overall measure
// can weigh steps before they start. A step still pending takes the new
// expectation; Start replaces it with the real figures.
func PlanSteps(ctx context.Context, steps ...Step) {
	r := From(ctx)
	if r == nil {
		return
	}
	parent := Current(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range steps {
		p := r.find(parent, s.Name, false)
		if p == nil {
			p = r.add(parent, s.Name)
			p.planned = true
		}
		if p.state == Pending {
			p.unit = s.Unit
			p.total.Store(s.Total)
		}
	}
}

// Unplan removes pending phases as if they had never been planned: work
// that turned out not to be needed (the content was already stored) leaves
// no "not run" rows. Phases that started stay.
func Unplan(ctx context.Context, names ...string) {
	r := From(ctx)
	if r == nil {
		return
	}
	parent := Current(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	list := &r.phases
	if parent != nil {
		list = &parent.kids
	}
	*list = slices.DeleteFunc(*list, func(p *Phase) bool {
		return p.state == Pending && slices.Contains(names, p.name)
	})
}

// Skip shows that a planned phase will not run, and why.
func Skip(ctx context.Context, name, reason string) {
	r := From(ctx)
	if r == nil {
		return
	}
	parent := Current(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.find(parent, name, true)
	if p == nil {
		p = r.add(parent, name)
	}
	p.state, p.reason = Skipped, reason
	p.started, p.ended = r.now(), r.now()
	r.event(p, true)
}

// Start begins a phase under the current one: a pending phase of that name
// if Plan listed it, otherwise a new one. total may be Unknown. It returns
// nil without a reporter; a nil phase ignores everything.
func Start(ctx context.Context, name string, unit Unit, total int64) *Phase {
	r := From(ctx)
	if r == nil {
		return nil
	}
	parent := Current(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.find(parent, name, true)
	if p == nil {
		p = r.insert(parent, name)
	}
	p.unit = unit
	p.total.Store(total)
	p.state = Running
	p.started = r.now()
	r.event(p, false)
	return p
}

// Tally returns the command-wide meter called name, made on first use. The
// footer shows tallies when the command ends.
func Tally(ctx context.Context, name string, unit Unit) *Meter {
	r := From(ctx)
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.tallies {
		if m.name == name {
			return m
		}
	}
	m := &Meter{name: name, unit: unit}
	r.tallies = append(r.tallies, m)
	return m
}

// Hint adds a line of advice to the footer, once.
func Hint(ctx context.Context, text string) {
	r := From(ctx)
	if r == nil || text == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.hints {
		if h == text {
			return
		}
	}
	r.hints = append(r.hints, text)
}

// Phase is one step of a command.
type Phase struct {
	r      *Reporter
	id     uint64
	parent *Phase
	name   string

	done, total atomic.Int64
	note, item  atomic.Pointer[string]

	// Guarded by r.mu.
	planned        bool // listed by Plan before it started
	unit           Unit
	state          State
	warn, reason   string
	started, ended time.Time
	kids           []*Phase
	meters         []*Meter
}

// Add counts n more units done. It is safe from any goroutine and lock-free.
func (p *Phase) Add(n int64) {
	if p != nil {
		p.done.Add(n)
	}
}

// Total is the phase's total so far (Unknown when not known).
func (p *Phase) Total() int64 {
	if p == nil {
		return Unknown
	}
	return p.total.Load()
}

// SetTotal sets the phase's total once it is known (Unknown until then).
func (p *Phase) SetTotal(n int64) {
	if p != nil {
		p.total.Store(n)
	}
}

// Note says what the phase works on now, such as a partition. The text may
// come from evidence; the renderer sanitises it.
func (p *Phase) Note(s string) {
	if p != nil {
		p.note.Store(&s)
	}
}

// Item says which item the phase works on now, such as the file it reads;
// "" clears it. It is lock-free but allocates, so a hot loop calls it only
// now and then. The text may come from evidence; the renderer sanitises it.
func (p *Phase) Item(s string) {
	if p != nil {
		p.item.Store(&s)
	}
}

// Warn records something that needs attention: a later Done(nil) ends the
// phase as a warning.
func (p *Phase) Warn(msg string) {
	if p == nil {
		return
	}
	p.r.mu.Lock()
	p.warn = msg
	p.r.mu.Unlock()
}

// Meter returns the phase's second count called name, made on first use.
func (p *Phase) Meter(name string, unit Unit) *Meter {
	if p == nil {
		return nil
	}
	p.r.mu.Lock()
	defer p.r.mu.Unlock()
	for _, m := range p.meters {
		if m.name == name {
			return m
		}
	}
	m := &Meter{name: name, unit: unit}
	p.meters = append(p.meters, m)
	return m
}

// Done ends the phase; the first call wins. nil ends it OK (or as a warning
// after Warn). A WarningError ends it as a warning. A signal, or the
// command's context being cancelled, ends it interrupted. Anything else
// ends it failed, keeping the error's text; that includes a bare
// context.Canceled while the command goes on, such as the index pass
// stopped because storage failed. Done never fills the count up to the
// total: a bar that stopped at 63% says so.
func (p *Phase) Done(err error) {
	if p == nil {
		return
	}
	r := p.r
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.state.Ended() {
		return
	}
	p.state = r.classify(err, p.warn != "")
	if err != nil && p.state != Warning {
		p.reason = err.Error()
	} else if err != nil && p.warn == "" {
		p.warn = err.Error()
	}
	p.ended = r.now()
	r.event(p, true)
}

// Meter is a second count: bytes read by a pass that counts files, chunks
// that were new, relations added.
type Meter struct {
	name string
	unit Unit
	v    atomic.Int64
}

// Add counts n more; safe from any goroutine and lock-free.
func (m *Meter) Add(n int64) {
	if m != nil {
		m.v.Add(n)
	}
}

// Reporter collects one command's progress.
type Reporter struct {
	mu    sync.Mutex
	root  context.Context
	clock func() time.Time
	start time.Time

	nextID  uint64
	phases  []*Phase
	tallies []*Meter
	hints   []string

	seq     uint64
	events  []Event
	dropped uint64

	outcome  State
	finished bool
}

// eventCap bounds the event ring: the plain renderer reads it often, and a
// folder of many small files must not grow it without end.
const eventCap = 1024

// New returns a reporter for the command running under ctx. now is the
// clock; nil means time.Now.
func New(ctx context.Context, now func() time.Time) *Reporter {
	if now == nil {
		now = time.Now
	}
	return &Reporter{root: ctx, clock: now, start: now()}
}

func (r *Reporter) now() time.Time { return r.clock() }

// add appends a pending phase under parent (nil: top level).
func (r *Reporter) add(parent *Phase, name string) *Phase {
	r.nextID++
	p := &Phase{r: r, id: r.nextID, parent: parent, name: name}
	p.total.Store(Unknown)
	if parent == nil {
		r.phases = append(r.phases, p)
	} else {
		parent.kids = append(parent.kids, p)
	}
	return p
}

// insert adds a phase that was not planned before the first pending
// sibling, so the display reads in the order things happen.
func (r *Reporter) insert(parent *Phase, name string) *Phase {
	p := r.add(parent, name)
	list := &r.phases
	if parent != nil {
		list = &parent.kids
	}
	ps := *list
	for i, q := range ps[:len(ps)-1] {
		if q.state == Pending {
			copy(ps[i+1:], ps[i:len(ps)-1])
			ps[i] = p
			break
		}
	}
	return p
}

// find returns the phase called name under parent; with pendingOnly, only
// one that has not started.
func (r *Reporter) find(parent *Phase, name string, pendingOnly bool) *Phase {
	list := r.phases
	if parent != nil {
		list = parent.kids
	}
	for _, p := range list {
		if p.name == name && (!pendingOnly || p.state == Pending) {
			return p
		}
	}
	return nil
}

func (r *Reporter) classify(err error, warned bool) State {
	var sig *lifecycle.SignalError
	switch {
	case err == nil && warned:
		return Warning
	case err == nil:
		return OK
	case errors.As(err, &sig), r.root != nil && r.root.Err() != nil:
		return Interrupted
	case lifecycle.IsWarning(err):
		return Warning
	}
	return Failed
}

// event records a phase starting or ending.
func (r *Reporter) event(p *Phase, ended bool) {
	r.seq++
	var path []string
	for q := p.parent; q != nil; q = q.parent {
		path = append([]string{q.name}, path...)
	}
	r.events = append(r.events, Event{Seq: r.seq, Ended: ended, Path: path, Phase: p.view(false)})
	if over := len(r.events) - eventCap; over > 0 {
		r.events = append(r.events[:0:0], r.events[over:]...)
		r.dropped += uint64(over)
	}
}

// Finish ends the command: phases still running take the command's
// outcome, phases still pending are skipped ("not run").
func (r *Reporter) Finish(err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	r.finished = true
	r.outcome = r.classify(err, false)
	if r.outcome == OK && r.anyWarned(r.phases) {
		r.outcome = Warning
	}
	var settle func([]*Phase)
	settle = func(ps []*Phase) {
		for _, p := range ps {
			switch p.state {
			case Running:
				p.state, p.ended = r.outcome, r.now()
				if err != nil && p.reason == "" {
					p.reason = err.Error()
				}
				r.event(p, true)
			case Pending:
				p.state, p.reason = Skipped, "not run"
				p.started, p.ended = r.now(), r.now()
			}
			settle(p.kids)
		}
	}
	settle(r.phases)
}

func (r *Reporter) anyWarned(ps []*Phase) bool {
	for _, p := range ps {
		if p.state == Warning || r.anyWarned(p.kids) {
			return true
		}
	}
	return false
}

// View is a phase as a renderer sees it.
type View struct {
	ID             uint64
	Name           string
	Unit           Unit
	State          State
	Planned        bool  // listed by Plan before it started
	Done, Total    int64 // a pending phase's Total is what it expects
	Note           string
	Item           string // what it works on now, such as a file
	Warn           string
	Reason         string // why it failed, was interrupted or skipped
	Started, Ended time.Time
	Meters         []MeterView
	Kids           []View
}

// MeterView is a meter's name and value.
type MeterView struct {
	Name  string
	Unit  Unit
	Value int64
}

// Event is a phase starting or ending. Path names its ancestors.
type Event struct {
	Seq   uint64
	Ended bool
	Path  []string
	Phase View
}

// Snapshot is everything a renderer draws at one moment.
type Snapshot struct {
	Seq      uint64
	Start    time.Time
	Phases   []View
	Events   []Event // after the sequence number asked for
	Dropped  uint64  // events lost from the ring
	Tallies  []MeterView
	Hints    []string
	Finished bool
	Outcome  State
}

// view copies p; with kids, its children too. A finished group (a phase
// with no unit of its own, such as one evidence of a folder) is folded:
// its children are dropped once it has ended OK, so a folder of many files
// keeps a short display and bounded memory.
func (p *Phase) view(kids bool) View {
	v := View{
		ID: p.id, Name: p.name, Unit: p.unit, State: p.state, Planned: p.planned,
		Done: p.done.Load(), Total: p.total.Load(),
		Warn: p.warn, Reason: p.reason, Started: p.started, Ended: p.ended,
	}
	if n := p.note.Load(); n != nil {
		v.Note = *n
	}
	if n := p.item.Load(); n != nil {
		v.Item = *n
	}
	for _, m := range p.meters {
		v.Meters = append(v.Meters, MeterView{m.name, m.unit, m.v.Load()})
	}
	if kids && !(p.unit == None && p.state == OK) {
		for _, k := range p.kids {
			v.Kids = append(v.Kids, k.view(true))
		}
	}
	return v
}

// Snapshot returns the current state and the events after seq after.
func (r *Reporter) Snapshot(after uint64) Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Snapshot{Seq: r.seq, Start: r.start, Dropped: r.dropped, Finished: r.finished, Outcome: r.outcome}
	for _, p := range r.phases {
		s.Phases = append(s.Phases, p.view(true))
	}
	for _, e := range r.events {
		if e.Seq > after {
			s.Events = append(s.Events, e)
		}
	}
	for _, m := range r.tallies {
		s.Tallies = append(s.Tallies, MeterView{m.name, m.unit, m.v.Load()})
	}
	s.Hints = append(s.Hints, r.hints...)
	return s
}

// Prune drops ended top-level groups' children for good, so a long folder
// store does not keep every file's phases. The renderer calls it after it
// has shown their end.
func (r *Reporter) Prune() {
	r.mu.Lock()
	defer r.mu.Unlock()
	var prune func([]*Phase)
	prune = func(ps []*Phase) {
		for _, p := range ps {
			if p.unit == None && p.state == OK {
				p.kids = nil
				continue
			}
			prune(p.kids)
		}
	}
	prune(r.phases)
}
