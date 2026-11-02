package progress

import (
	"errors"
	"math"
	"slices"
	"testing"
	"time"
)

func (c *clock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// estimator measures r's snapshots at c's time.
func estimator(r *Reporter, c *clock) func() Estimate {
	tr := NewTracker()
	return func() Estimate { return tr.Update(r.Snapshot(0), c.now()) }
}

func TestRate(t *testing.T) {
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	v := View{State: Running, Started: t0, Total: 1000, Done: 100}
	var r Rate
	// The mean rate for the first two seconds, then a weighted average.
	if got := r.Sample(v, t0.Add(time.Second)); !near(got, 100) {
		t.Errorf("seed rate %v, want 100", got)
	}
	v.Done = 300
	if got := r.Sample(v, t0.Add(3*time.Second)); !near(got, 100) {
		t.Errorf("steady rate %v, want 100", got)
	}
	v.Done = 900
	got := r.Sample(v, t0.Add(4*time.Second))
	if want := 100 + (1-math.Exp(-0.2))*500; !near(got, want) {
		t.Errorf("rate %v, want %v", got, want)
	}
	now := t0.Add(4 * time.Second)
	if d, ok := ETA(v, got, now); !ok || d != time.Duration(100/got*float64(time.Second)) {
		t.Errorf("ETA %v, %v", d, ok)
	}
	for name, c := range map[string]struct {
		v   View
		now time.Time
	}{
		"under 2 s":     {v, t0.Add(time.Second)},
		"under 1%":      {View{State: Running, Started: t0, Total: 1000, Done: 9}, now},
		"unknown total": {View{State: Running, Started: t0, Total: Unknown, Done: 9}, now},
		"ended":         {View{State: OK, Started: t0, Total: 1000, Done: 900}, now},
	} {
		if _, ok := ETA(c.v, 100, c.now); ok {
			t.Errorf("%s: an ETA", name)
		}
	}
	if _, ok := ETA(v, 0, now); ok {
		t.Error("an ETA at rate 0")
	}
	if got := MeanRate(View{State: OK, Done: 500, Started: t0, Ended: t0.Add(5 * time.Second)}); got != 100 {
		t.Errorf("mean rate %v", got)
	}
}

func TestWeigh(t *testing.T) {
	for _, c := range []struct {
		v              View
		weight, credit int64
		ok             bool
	}{
		{View{Unit: Bytes, State: Pending, Total: 100}, 100, 0, true},
		{View{Unit: Bytes, State: Pending, Total: Unknown}, 0, 0, false},
		{View{Unit: Bytes, State: Running, Total: 100, Done: 140}, 100, 100, true},
		{View{Unit: Bytes, State: OK, Total: 100, Done: 60}, 100, 100, true},
		{View{Unit: Bytes, State: OK, Total: Unknown, Done: 60}, 60, 60, true},
		{View{Unit: Bytes, State: Failed, Total: 100, Done: 63}, 100, 63, true},
		{View{Unit: Bytes, State: Interrupted, Total: 100, Done: 10}, 100, 10, true},
		{View{Unit: Bytes, State: Skipped, Total: 100}, 0, 0, false},
		{View{Unit: Files, State: Running, Total: 100}, 0, 0, false},
	} {
		w, cr, ok := weigh(&c.v)
		if w != c.weight || cr != c.credit || ok != c.ok {
			t.Errorf("%+v: %d, %d, %v; want %d, %d, %v", c.v, w, cr, ok, c.weight, c.credit, c.ok)
		}
	}
}

// The default store at a series of moments: identify, then store with the
// index beside it, then the index's tail and compact.
func TestEstimateDefaultStore(t *testing.T) {
	ctx, r, c := newTest(t)
	est := estimator(r, c)
	const size = 400 << 20
	PlanSteps(ctx, Step{"identify", Bytes, size}, Step{"store", Bytes, size}, Step{"index", Files, Unknown})

	if e := est(); e.Mode != ModeNone {
		t.Fatalf("planned, nothing started: %+v", e)
	}

	id := Start(ctx, "identify", Bytes, size)
	c.advance(time.Second)
	id.Add(size / 4)
	if e := est(); e.Mode != ModeWork || e.Step != 1 || e.Focus.Name != "identify" || !near(e.Fraction, 0.125) || e.HasETA {
		t.Fatalf("identify after 1s: %+v", e)
	}
	c.advance(2 * time.Second)
	id.Add(size / 4)
	// Store is still to come, so the ETA is identify's alone.
	if e := est(); !e.HasETA || e.Whole || e.Approx || e.Rate <= 0 {
		t.Fatalf("identify after 3s: %+v", e)
	}
	id.Add(size / 2)
	id.Done(nil)

	st := Start(ctx, "store", Bytes, size)
	ix := Start(ctx, "index", Files, Unknown)
	c.advance(time.Second)
	st.Add(size / 4)
	ix.Add(1000)
	if e := est(); e.Mode != ModeWork || e.Step != 2 || e.Focus.Name != "store" || !near(e.Fraction, 0.625) || e.HasETA || !e.Approx {
		t.Fatalf("store after 1s: %+v", e)
	}
	c.advance(2 * time.Second)
	st.Add(size / 4)
	if e := est(); !e.HasETA || !e.Whole || !e.Approx || e.ETA <= 0 {
		t.Fatalf("store after 3s: %+v", e)
	}
	st.Add(size / 2)
	// All bytes counted but the step still runs: under 100%.
	if e := est(); e.Fraction != 0.99 {
		t.Fatalf("store counted out: fraction %v", e.Fraction)
	}
	st.Done(nil)

	if e := est(); e.Mode != ModeActivity || e.Focus.Name != "index" || e.Done != 1000 {
		t.Fatalf("index tail: %+v", e)
	}
	ix.Done(nil)
	cp := Start(ctx, "compact", None, 0)
	if e := est(); e.Mode != ModeActivity || e.Focus.Name != "compact" {
		t.Fatalf("compact: %+v", e)
	}
	cp.Done(nil)
	c.advance(time.Second)
	if e := est(); e.Mode != ModeNone || e.Elapsed <= 0 {
		t.Fatalf("after compact: %+v", e)
	}
}

// An alias store stops after identify; the steps it planned go, and the
// estimate has nothing left to measure.
func TestEstimateAlias(t *testing.T) {
	ctx, r, c := newTest(t)
	est := estimator(r, c)
	PlanSteps(ctx, Step{"identify", Bytes, 10}, Step{"store", Bytes, 10}, Step{"index", Files, Unknown})
	Start(ctx, "identify", Bytes, 10).Done(nil)
	Unplan(ctx, "store", "index")
	if e := est(); e.Mode != ModeNone {
		t.Fatalf("after the alias: %+v", e)
	}
}

// Under the async strategy identify is not planned: it starts before the
// pending store and is a work step all the same.
func TestEstimateAsyncIdentify(t *testing.T) {
	ctx, r, c := newTest(t)
	est := estimator(r, c)
	PlanSteps(ctx, Step{"store", Bytes, 100}, Step{"index", Files, Unknown})
	Start(ctx, "identify", Bytes, 100).Add(50)
	if e := est(); e.Mode != ModeWork || e.Steps != 2 || e.Step != 1 || e.Focus.Name != "identify" || !near(e.Fraction, 0.25) {
		t.Fatalf("%+v", e)
	}
}

// With --no-index the store is the only step.
func TestEstimateNoIndex(t *testing.T) {
	ctx, r, c := newTest(t)
	est := estimator(r, c)
	PlanSteps(ctx, Step{"store", Bytes, 100}, Step{"index", Files, Unknown})
	Skip(ctx, "index", "--no-index")
	st := Start(ctx, "store", Bytes, 100)
	c.advance(3 * time.Second)
	st.Add(50)
	if e := est(); e.Mode != ModeWork || e.Steps != 1 || !e.Whole || !e.HasETA || e.Approx {
		t.Fatalf("%+v", e)
	}
}

// Work found late (partition hashes) holds the bar rather than pulling it
// back; with no byte work still to come it is a step of its own.
func TestEstimatePartitionHashes(t *testing.T) {
	for _, enrich := range []bool{false, true} {
		ctx, r, c := newTest(t)
		est := estimator(r, c)
		steps := []Step{{"store", Bytes, 100}, {"index", Files, Unknown}}
		if enrich {
			steps = append(steps, Step{"entropy", Bytes, 100}, Step{"graph", Files, Unknown})
		}
		PlanSteps(ctx, steps...)
		st := Start(ctx, "store", Bytes, 100)
		st.Add(100)
		c.advance(time.Second)
		before := est().Fraction
		st.Done(nil)
		c.advance(time.Second)
		ph := Start(ctx, "partition hashes", Bytes, 50)
		e := est()
		if !enrich {
			if e.Mode != ModeStep || e.Focus.Name != "partition hashes" || e.Fraction != 0 {
				t.Errorf("without entropy: %+v", e)
			}
			continue
		}
		if e.Mode != ModeWork || e.Steps != 3 || e.Fraction < before {
			t.Errorf("with entropy: partition hashes pulled the bar back from %v: %+v", before, e)
		}
		ph.Add(50)
		if e2 := est(); !near(e2.Fraction, 150.0/250) {
			t.Errorf("with entropy, partition hashes done: %+v", e2)
		}
	}
}

// --fts --enrich: while entropy waits, the bar holds and the focus moves to
// the index and full-text; the planned entropy's real total replaces the
// evidence size; graph is a step of its own.
func TestEstimateFeatures(t *testing.T) {
	ctx, r, c := newTest(t)
	est := estimator(r, c)
	PlanSteps(ctx, Step{"store", Bytes, 100}, Step{"index", Files, Unknown}, Step{"full-text", Files, Unknown},
		Step{"entropy", Bytes, 100}, Step{"graph", Files, Unknown})
	st := Start(ctx, "store", Bytes, 100)
	ix := Start(ctx, "index", Files, Unknown)
	c.advance(3 * time.Second)
	st.Add(50)
	if e := est(); e.Mode != ModeWork || e.Steps != 2 || e.Step != 1 || !e.HasETA || e.Whole {
		t.Fatalf("store: %+v", e)
	}
	st.Add(50)
	st.Done(nil)
	if e := est(); e.Mode != ModeWork || e.Focus.Name != "index" || e.Step != 2 || e.HasETA || !near(e.Fraction, 0.5) {
		t.Fatalf("index while entropy waits: %+v", e)
	}
	ix.Done(nil)
	ft := Start(ctx, "full-text", Files, Unknown)
	ft.SetTotal(10)
	if e := est(); e.Mode != ModeWork || e.Focus.Name != "full-text" || !near(e.Fraction, 0.5) {
		t.Fatalf("full-text while entropy waits: %+v", e)
	}
	ft.Add(10)
	ft.Done(nil)
	en := Start(ctx, "entropy", Bytes, 80)
	if e := est(); e.Mode != ModeWork || e.Focus.Name != "entropy" || e.Step != 2 || !near(e.Fraction, 100.0/180) {
		t.Fatalf("entropy: %+v", e)
	}
	en.Add(80)
	en.Done(nil)
	gr := Start(ctx, "graph", Files, 20)
	gr.Add(5)
	if e := est(); e.Mode != ModeStep || e.Focus.Name != "graph" || !near(e.Fraction, 0.25) || !e.Whole {
		t.Fatalf("graph: %+v", e)
	}
}

// Work that has not begun does not make a bar: a reindex with --enrich
// shows its index pass as activity, not as 0% of the entropy to come.
func TestEstimateWorkNotBegun(t *testing.T) {
	ctx, r, c := newTest(t)
	est := estimator(r, c)
	PlanSteps(ctx, Step{"delete old index", Files, Unknown}, Step{"index", Files, Unknown},
		Step{"entropy", Bytes, 100}, Step{"graph", Files, Unknown})
	Start(ctx, "delete old index", Files, Unknown).Done(nil)
	Start(ctx, "index", Files, Unknown).Add(10)
	if e := est(); e.Mode != ModeActivity || e.Focus.Name != "index" {
		t.Fatalf("%+v", e)
	}
}

// A folder: items done plus the running item's share of its byte work, an
// ETA from the pace so far, and no dip while the counter catches up.
func TestEstimateItems(t *testing.T) {
	ctx, r, c := newTest(t)
	est := estimator(r, c)
	all := Start(ctx, "evidence", Evidence, 4)
	if e := est(); e.Mode != ModeStep || e.Focus.Name != "evidence" {
		t.Fatalf("before the first item: %+v", e)
	}
	one := Start(Within(ctx, all), "a.bin", None, 0)
	ictx := Within(ctx, one)
	PlanSteps(ictx, Step{"store", Bytes, 100}, Step{"index", Files, Unknown})
	st := Start(ictx, "store", Bytes, 100)
	c.advance(time.Second)
	st.Add(50)
	e := est()
	if e.Mode != ModeItems || e.Group.Name != "evidence" || e.Item.Name != "a.bin" || e.Focus.Name != "store" ||
		!near(e.ItemFraction, 0.5) || !near(e.Fraction, 0.125) || e.HasETA {
		t.Fatalf("first item: %+v", e)
	}
	if !slices.Equal(e.FocusPath, []string{"evidence", "a.bin"}) {
		t.Errorf("focus path %q", e.FocusPath)
	}
	c.advance(2 * time.Second)
	if e := est(); !e.HasETA || !e.Approx || !e.Whole || e.ETA != 21*time.Second {
		t.Fatalf("first item after 3s: %+v", e)
	}
	st.Add(50)
	st.Done(nil)
	if e := est(); !near(e.ItemFraction, 1) || !near(e.Fraction, 0.25) {
		t.Fatalf("first item's bytes done: %+v", e)
	}
	one.Done(nil)
	// The item ended but the counter has not moved yet.
	if e := est(); e.Mode != ModeItems || e.Item != nil || !near(e.Fraction, 0.25) {
		t.Fatalf("between items: %+v", e)
	}
	all.Add(1)
	if e := est(); !near(e.Fraction, 0.25) {
		t.Fatalf("one of four: %+v", e)
	}

	// One evidence (a single reindex): the item's own steps.
	ctx, r, c = newTest(t)
	est = estimator(r, c)
	all = Start(ctx, "evidence", Evidence, 1)
	one = Start(Within(ctx, all), "case.e01", None, 0)
	ictx = Within(ctx, one)
	Plan(ictx, "delete old index", "index")
	Start(ictx, "delete old index", Files, Unknown).Done(nil)
	Start(ictx, "index", Files, Unknown).Add(10)
	e = est()
	if e.Mode != ModeActivity || e.Focus.Name != "index" || !slices.Equal(e.FocusPath, []string{"evidence", "case.e01"}) {
		t.Fatalf("single item: %+v", e)
	}
}

// Items weighed by size: each moves the bar by its share of the weight, not
// by one of N, so a large evidence among small ones holds the bar as long
// as it takes. An item's weight counts as done when it ends, however it
// ends.
func TestEstimateItemsWeighed(t *testing.T) {
	ctx, r, c := newTest(t)
	est := estimator(r, c)
	all := Start(ctx, "evidence", Evidence, 3)
	all.Weigh(1000)
	item := func(name string, weight, size int64) (*Phase, *Phase) {
		one := Start(Within(ctx, all), name, None, 0)
		one.Weigh(weight)
		ictx := Within(ctx, one)
		PlanSteps(ictx, Step{"store", Bytes, size})
		return one, Start(ictx, "store", Bytes, size)
	}
	one, st := item("small", 100, 10)
	st.Add(10)
	st.Done(nil)
	one.Done(nil)
	// The weight is done with the item, before the counter moves.
	if e := est(); e.Mode != ModeItems || e.Item != nil || !near(e.Fraction, 0.1) {
		t.Fatalf("small item done: %+v", e)
	}
	all.Add(1)
	one, st = item("big", 800, 100)
	c.advance(10 * time.Second)
	st.Add(25)
	e := est()
	if e.Mode != ModeItems || e.Item.Name != "big" || e.Done != 1 || !near(e.ItemFraction, 0.25) || !near(e.Fraction, 0.3) {
		t.Fatalf("big item a quarter done: %+v", e)
	}
	// The pace so far over the weight, not over the items: by count the
	// bar would read 42%.
	if want := time.Duration(float64(10*time.Second) * (1 - e.Fraction) / e.Fraction); !e.HasETA || e.ETA != want || e.ETA < 23*time.Second {
		t.Fatalf("big item ETA: %+v", e)
	}
	st.Add(75)
	st.Done(nil)
	one.Done(nil)
	all.Add(1)
	if e := est(); !near(e.Fraction, 0.9) {
		t.Fatalf("big item done: %+v", e)
	}
	failed := errors.New("read error")
	one, st = item("last", 100, 10)
	st.Done(failed)
	one.Done(failed)
	all.Add(1)
	if g := r.Snapshot(0).Phases[0]; g.Weight != 1000 || g.WeightDone != 1000 || g.Kids[1].Weight != 800 {
		t.Fatalf("weights after a failed item: group %d, done %d, big %d", g.Weight, g.WeightDone, g.Kids[1].Weight)
	}

	// An item still running when the command ends has its weight done too.
	ctx, r, _ = newTest(t)
	all = Start(ctx, "evidence", Evidence, 1)
	all.Weigh(40)
	Start(Within(ctx, all), "a.bin", None, 0).Weigh(40)
	r.Finish(failed)
	if g := r.Snapshot(0).Phases[0]; g.WeightDone != 40 {
		t.Fatalf("weight done after Finish: %d", g.WeightDone)
	}
	(*Phase)(nil).Weigh(1)
}

// repair --upgrade --verify: upgrade counts files, so it measures itself;
// verify, the only byte work, is the work, and starts a bar of its own.
func TestEstimateRepair(t *testing.T) {
	ctx, r, c := newTest(t)
	est := estimator(r, c)
	up := Start(ctx, "upgrade", Files, 10)
	up.Add(9)
	if e := est(); e.Mode != ModeStep || !near(e.Fraction, 0.9) {
		t.Fatalf("upgrade: %+v", e)
	}
	up.Add(1)
	up.Done(nil)
	c.advance(time.Second)
	vf := Start(ctx, "verify", Bytes, 100)
	vf.Add(10)
	if e := est(); e.Mode != ModeWork || e.Steps != 1 || e.Focus.Name != "verify" || !near(e.Fraction, 0.1) {
		t.Fatalf("verify: %+v", e)
	}
}

// A total that grows while the phase runs (micro extract finds files as it
// goes) has no fraction.
func TestEstimateGrowing(t *testing.T) {
	ctx, r, c := newTest(t)
	est := estimator(r, c)
	m := Start(ctx, "micro", Files, 0)
	if e := est(); e.Mode != ModeActivity {
		t.Fatalf("no total yet: %+v", e)
	}
	m.SetTotal(10)
	m.Add(5)
	if e := est(); e.Mode != ModeStep || !near(e.Fraction, 0.5) {
		t.Fatalf("first total: %+v", e)
	}
	m.SetTotal(20)
	if e := est(); e.Mode != ModeActivity || !e.Growing {
		t.Fatalf("grown: %+v", e)
	}
}

// The tracker keeps rates only for running phases.
func TestTrackerForgetsEndedPhases(t *testing.T) {
	ctx, r, c := newTest(t)
	tr := NewTracker()
	p := Start(ctx, "store", Bytes, 100)
	c.advance(time.Second)
	p.Add(10)
	tr.Update(r.Snapshot(0), c.now())
	if tr.Rate(p.id) != 10 {
		t.Fatalf("rate %v", tr.Rate(p.id))
	}
	p.Done(nil)
	tr.Update(r.Snapshot(0), c.now())
	if tr.Rate(p.id) != 0 || len(tr.phases) != 0 {
		t.Fatalf("an ended phase is still tracked: %v", tr.phases)
	}
}

// A frame of a steady display allocates nothing.
func TestUpdateAllocatesNothing(t *testing.T) {
	ctx, r, c := newTest(t)
	tr := NewTracker()
	PlanSteps(ctx, Step{"identify", Bytes, 100}, Step{"store", Bytes, 100}, Step{"index", Files, Unknown})
	Start(ctx, "identify", Bytes, 100).Done(nil)
	Start(ctx, "store", Bytes, 100)
	Start(ctx, "index", Files, Unknown)
	snap := r.Snapshot(0)
	now := c.now()
	tr.Update(snap, now)
	if n := testing.AllocsPerRun(100, func() {
		now = now.Add(100 * time.Millisecond)
		snap.Phases[1].Done++
		tr.Update(snap, now)
	}); n != 0 {
		t.Errorf("Update allocates %v times a frame", n)
	}
}
