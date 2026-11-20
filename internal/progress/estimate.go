package progress

import (
	"math"
	"time"
)

// This file measures a command as a whole for renderers: each running
// phase's rate, and how far the command has got. It draws nothing and
// starts no goroutine; a renderer calls Tracker.Update once a frame.

// Rate estimates a phase's rate: the mean rate for its first two seconds,
// then an exponentially weighted average over about five seconds.
type Rate struct {
	lastDone int64
	lastT    time.Time
	v        float64
}

const (
	rateSeed = 2 * time.Second
	rateTau  = 5.0 // seconds
)

// Sample folds v's count at now into the estimate and returns it.
func (e *Rate) Sample(v View, now time.Time) float64 {
	if e.lastT.IsZero() {
		e.lastT, e.lastDone = v.Started, 0
	}
	dt := now.Sub(e.lastT).Seconds()
	if dt <= 0 {
		return e.v
	}
	if now.Sub(v.Started) < rateSeed {
		e.v = float64(v.Done) / now.Sub(v.Started).Seconds()
	} else {
		inst := float64(v.Done-e.lastDone) / dt
		e.v += (1 - math.Exp(-dt/rateTau)) * (inst - e.v)
	}
	e.lastT, e.lastDone = now, v.Done
	return e.v
}

// ETA is the time v has left at rate, when that means something: a running
// phase with a known total, two seconds of history, at least 1% done and a
// rate above zero.
func ETA(v View, rate float64, now time.Time) (time.Duration, bool) {
	if v.State != Running || v.Total <= 0 || rate <= 0 || now.Sub(v.Started) < rateSeed || v.Done*100 < v.Total {
		return 0, false
	}
	return time.Duration(float64(max(v.Total-v.Done, 0)) / rate * float64(time.Second)), true
}

// MeanRate is an ended phase's average rate.
func MeanRate(v View) float64 {
	d := v.Ended.Sub(v.Started).Seconds()
	if d <= 0 {
		return 0
	}
	return float64(v.Done) / d
}

// Mode is how an Estimate measures the command.
type Mode uint8

const (
	// ModeNone: nothing runs.
	ModeNone Mode = iota
	// ModeWork: the work steps, the steps that count bytes, as one
	// fraction of all their bytes, those still to come included. A step's
	// bytes count times its Cost.
	ModeWork
	// ModeItems: a folder, or several evidence: items done, plus the
	// running item's share of its own work. Items the group weighs (by
	// size) count by their weight, not one each.
	ModeItems
	// ModeStep: one running step of known size.
	ModeStep
	// ModeActivity: work of unknown size, so no fraction.
	ModeActivity
)

// Estimate is the command as a whole at one moment, for an overall row.
type Estimate struct {
	Mode Mode
	// Fraction is how much is done, 0 to 1. Within one mode and scope it
	// never goes back, and it stays under 1 until the command ends. It is
	// 0 in Activity and None.
	Fraction float64
	// Done and Total, in Unit, are what Fraction measures: the bytes of
	// the work steps (each step's weighed by its Cost), the items, or
	// Focus's own count.
	Done, Total int64
	Unit        Unit
	// Focus is the step the row talks about; FocusPath names the phases
	// above it. InWork: Focus is one of the work steps.
	Focus     View
	FocusPath []string
	InWork    bool
	// Scope is the list of steps measured: the top level, or one item's.
	Scope []View
	// In Items mode, Group counts the items, Item is the running one the row
	// follows, the one started last (nil between two), and ItemFraction is
	// how much of its byte work is done. Others are the other items running
	// beside it, when a folder stores several at once.
	Group, Item  *View
	ItemFraction float64
	Others       []*View
	// Rate is Focus's units per second.
	Rate float64
	// ETA is the time left: for the rest of the command when Whole, for
	// Focus alone otherwise. Approx marks an ETA other running work may
	// move.
	ETA                   time.Duration
	HasETA, Whole, Approx bool
	// Growing: Focus's total grew while it ran (what it counts is still
	// being found), so it has no fraction.
	Growing bool
	// Elapsed is how long Focus has run; in None mode, the command.
	Elapsed time.Duration
	// Step and Steps: in Work mode, Focus is the Step-th of Steps work
	// steps (or the next is, when Focus is not one).
	Step, Steps int
}

// Tracker follows one command's snapshots. It keeps the rate of each
// running phase and the fractions it has shown, so the overall fraction
// never goes back. It is not safe for concurrent use.
type Tracker struct {
	gen    uint64
	phases map[uint64]*tracked
	shown  map[shownKey]float64
	path   []string
}

type tracked struct {
	rate  Rate
	total int64 // the first known total, to tell when it grows
	gen   uint64
}

// shownKey is the scope a fraction was shown for: one mode over one list
// of steps, or over one step.
type shownKey struct {
	mode Mode
	id   uint64
}

// NewTracker returns a tracker for one command.
func NewTracker() *Tracker {
	return &Tracker{phases: map[uint64]*tracked{}, shown: map[shownKey]float64{}}
}

// Rate is the rate of phase id as of the last Update; 0 for a phase that
// was not running.
func (t *Tracker) Rate(id uint64) float64 {
	if p := t.phases[id]; p != nil {
		return p.rate.v
	}
	return 0
}

// Update samples every running phase of s, which was taken at now, and
// measures the command. The Estimate's FocusPath, Group and Item point into
// s and the tracker: they hold until the next Update.
func (t *Tracker) Update(s Snapshot, now time.Time) Estimate {
	t.gen++
	t.sample(s.Phases, now)
	for id, p := range t.phases {
		if p.gen != t.gen {
			delete(t.phases, id)
		}
	}
	t.path = t.path[:0]
	e := t.scope(s.Phases, 0, now, s.Finished)
	e.FocusPath = t.path
	if e.Scope == nil {
		e.Scope = s.Phases
	}
	if e.Mode == ModeNone {
		e.Elapsed = now.Sub(s.Start)
	}
	return e
}

func (t *Tracker) sample(vs []View, now time.Time) {
	for i := range vs {
		v := &vs[i]
		if v.State == Running {
			p := t.phases[v.ID]
			if p == nil {
				p = &tracked{total: v.Total}
				t.phases[v.ID] = p
			}
			if p.total <= 0 {
				p.total = v.Total
			}
			p.rate.Sample(*v, now)
			p.gen = t.gen
		}
		t.sample(v.Kids, now)
	}
}

// scope measures one list of sibling phases; key names the list (its
// parent's ID, 0 for the top level).
func (t *Tracker) scope(vs []View, key uint64, now time.Time, finished bool) Estimate {
	for i := range vs {
		if g := &vs[i]; g.State == Running && g.Total > 0 {
			if item, running, ok := itemsOf(g); ok {
				return t.items(vs, g, item, running, now, finished)
			}
		}
	}
	if e, ok := t.work(vs, key, now, finished); ok {
		return e
	}
	run := -1
	for i := range vs {
		v := &vs[i]
		if v.State != Running {
			continue
		}
		if v.Total > 0 && !t.growing(v) {
			return t.step(vs, i, now, finished)
		}
		if run < 0 || (vs[run].Unit == None && v.Unit != None) {
			run = i
		}
	}
	if run < 0 {
		return Estimate{Mode: ModeNone, Scope: vs}
	}
	v := &vs[run]
	return Estimate{
		Mode: ModeActivity, Scope: vs, Focus: *v, Unit: v.Unit, Done: v.Done, Total: v.Total,
		Rate: t.Rate(v.ID), Growing: t.growing(v), Elapsed: now.Sub(v.Started),
		Approx: othersRun(vs, run),
	}
}

// items measures g, one of scopeOf, a phase that counts items, with item
// running in it, among running.
func (t *Tracker) items(scopeOf []View, g, item *View, running []*View, now time.Time, finished bool) Estimate {
	t.path = append(t.path, g.Name)
	if item != nil {
		t.path = append(t.path, item.Name)
		if g.Total == 1 {
			return t.scope(item.Kids, item.ID, now, finished)
		}
	}
	e := Estimate{
		Mode: ModeItems, Group: g, Item: item, Scope: scopeOf,
		Unit: g.Unit, Done: min(g.Done, g.Total), Total: g.Total, Elapsed: now.Sub(g.Started),
	}
	if item != nil {
		sub := t.scope(item.Kids, item.ID, now, finished)
		e.Focus, e.Rate, e.Growing = sub.Focus, sub.Rate, sub.Growing
		e.ItemFraction = workFraction(item.Kids)
	}
	// Every running item's share counts: several run at once when a folder
	// stores in parallel.
	var share, weighed float64
	for _, k := range running {
		kf := workFraction(k.Kids)
		share += kf
		weighed += kf * float64(k.Weight)
		if k != item {
			e.Others = append(e.Others, k)
		}
	}
	f := (float64(e.Done) + share) / float64(g.Total)
	if g.Weight > 0 {
		// Items weighed by size: a large one holds the bar as long as it
		// takes, and small ones move it little.
		f = (float64(min(g.WeightDone, g.Weight)) + weighed) / float64(g.Weight)
	}
	e.Fraction = t.show(shownKey{ModeItems, g.ID}, f, finished)
	if e.Fraction >= 0.01 && e.Elapsed >= rateSeed {
		e.ETA = time.Duration(float64(e.Elapsed) * (1 - e.Fraction) / e.Fraction)
		e.HasETA, e.Whole, e.Approx = true, true, true
	}
	return e
}

// work is the Work measure of vs, when its work has begun and some of it is
// still to come.
func (t *Tracker) work(vs []View, key uint64, now time.Time, finished bool) (Estimate, bool) {
	var e Estimate
	var weight, credit float64
	focus, next, run, ended := -1, -1, -1, 0
	for i := range vs {
		v := &vs[i]
		if v.State == Running && run < 0 {
			run = i
		}
		if !inWork(vs, i) {
			continue
		}
		w, c := costed(v)
		weight += w
		credit += c
		e.Steps++
		switch v.State {
		case Running:
			if focus < 0 {
				focus, e.Step = i, e.Steps
			}
		case Pending:
			if next < 0 {
				next = i
			}
		default:
			ended++
		}
	}
	// The measure needs work under way, or done with more to come: work
	// not yet begun would hold a bar at 0% while something else runs.
	if focus < 0 && (next < 0 || ended == 0) {
		return Estimate{}, false
	}
	e.Mode, e.Scope, e.Unit, e.Done, e.Total = ModeWork, vs, Bytes, int64(credit), int64(weight)
	e.Fraction = t.show(shownKey{ModeWork, key}, credit/weight, finished)
	at := focus
	switch {
	case focus >= 0:
	case run >= 0:
		at, e.Step = run, ended+1
	default:
		at, e.Step = next, ended+1
	}
	f := &vs[at]
	e.Focus, e.Rate, e.Growing = *f, t.Rate(f.ID), t.growing(f)
	if f.State == Running {
		e.Elapsed = now.Sub(f.Started)
	}
	if at == focus {
		e.InWork = true
		e.ETA, e.HasETA = ETA(*f, e.Rate, now)
		e.Whole = ended == e.Steps-1
	}
	for i := range vs {
		if i != at && vs[i].State == Running && !inWork(vs, i) {
			e.Approx = true
		}
	}
	return e, true
}

// step is the Step measure of vs[i], a running step of known size.
func (t *Tracker) step(vs []View, i int, now time.Time, finished bool) Estimate {
	v := &vs[i]
	e := Estimate{
		Mode: ModeStep, Scope: vs, Focus: *v, Unit: v.Unit, Done: min(v.Done, v.Total), Total: v.Total,
		Rate: t.Rate(v.ID), Elapsed: now.Sub(v.Started), Whole: true, Approx: othersRun(vs, i),
	}
	e.Fraction = t.show(shownKey{ModeStep, v.ID}, float64(e.Done)/float64(v.Total), finished)
	e.ETA, e.HasETA = ETA(*v, e.Rate, now)
	return e
}

// show records f as shown for k and returns what to show: never less than
// before, and under 1 until the command has finished.
func (t *Tracker) show(k shownKey, f float64, finished bool) float64 {
	f = max(t.shown[k], min(f, 1))
	t.shown[k] = f
	if !finished {
		f = min(f, 0.99)
	}
	return f
}

// growing reports whether v's total grew while it ran.
func (t *Tracker) growing(v *View) bool {
	p := t.phases[v.ID]
	return p != nil && p.total > 0 && v.Total > p.total
}

// itemsOf finds g's running items when g counts items, whose phases are
// groups with no unit of their own: latest is the running kid started last,
// or nil between two, and running is every running kid. ok is false when g
// does not count items. A store of one item at a time has at most one
// running, at the end: the scan back stops there, so a long folder costs
// little; items run at once start one after another, so the scan stops
// after the first long run of ended items.
func itemsOf(g *View) (latest *View, running []*View, ok bool) {
	ended := 0
	for i := len(g.Kids) - 1; i >= 0; i-- {
		k := &g.Kids[i]
		switch {
		case k.Unit != None:
			return latest, running, ok
		case k.State == Running:
			ok, ended = true, 0
			running = append(running, k)
			if latest == nil {
				latest = k
			}
		case k.State.Ended():
			ok = true
			if ended++; ended > endedScan {
				return latest, running, ok
			}
		}
	}
	return latest, running, ok
}

// endedScan is how many ended items in a row itemsOf passes before it
// stops looking for running ones: more than a parallel store keeps between
// its oldest running item and its newest.
const endedScan = 4096

// weigh is a step's part in the Work measure: its weight (its total, or
// the total a pending step expects) and its credit (what it has done). ok
// is false for a step that counts no bytes or has no size.
func weigh(v *View) (weight, credit int64, ok bool) {
	if v.Unit != Bytes {
		return 0, 0, false
	}
	switch v.State {
	case Pending, Running:
		if v.Total <= 0 {
			return 0, 0, false
		}
		if v.State == Pending {
			return v.Total, 0, true
		}
		return v.Total, min(v.Done, v.Total), true
	case OK, Warning:
		if v.Total > 0 {
			return v.Total, v.Total, true
		}
		return v.Done, v.Done, v.Done > 0
	case Failed, Interrupted:
		// The bar stays where the step stopped.
		if v.Total > 0 {
			return v.Total, min(v.Done, v.Total), true
		}
		return v.Done, v.Done, v.Done > 0
	}
	return 0, 0, false
}

// costed is weigh's weight and credit for a work step v, each times v's
// cost.
func costed(v *View) (weight, credit float64) {
	w, c, _ := weigh(v)
	k := v.Cost
	if k <= 0 {
		k = 1
	}
	return float64(w) * k, float64(c) * k
}

// inWork reports whether vs[i] is a work step: byte work that was planned,
// that began while other byte work was still to come, or that is the only
// byte work. Byte work begun after all the rest (partition hashes without
// --enrich) is a step of its own.
func inWork(vs []View, i int) bool {
	v := &vs[i]
	if _, _, ok := weigh(v); !ok {
		return false
	}
	if v.Planned {
		return true
	}
	others := false
	for j := range vs {
		if j == i {
			continue
		}
		o := &vs[j]
		if _, _, ok := weigh(o); ok {
			others = true
			if !o.State.Ended() || o.Ended.After(v.Started) {
				return true
			}
		}
	}
	return !others
}

// workFraction is how much of vs's byte work is done; 0 with none.
func workFraction(vs []View) float64 {
	var weight, credit float64
	for i := range vs {
		if inWork(vs, i) {
			w, c := costed(&vs[i])
			weight += w
			credit += c
		}
	}
	if weight == 0 {
		return 0
	}
	return credit / weight
}

// othersRun reports whether a sibling of vs[i] runs too.
func othersRun(vs []View, i int) bool {
	for j := range vs {
		if j != i && vs[j].State == Running {
			return true
		}
	}
	return false
}
