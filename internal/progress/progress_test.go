package progress

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"indicer/internal/lifecycle"
)

// clock is a manual clock for tests.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func newTest(t *testing.T) (context.Context, *Reporter, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	r := New(context.Background(), c.now)
	return With(context.Background(), r), r, c
}

func TestNilSafety(t *testing.T) {
	var ctx context.Context
	if From(ctx) != nil || Current(ctx) != nil {
		t.Fatal("nil context has a reporter")
	}
	bg := context.Background()
	p := Start(bg, "x", Bytes, 10)
	if p != nil {
		t.Fatal("Start without a reporter returned a phase")
	}
	// None of these may panic.
	p.Add(1)
	p.SetTotal(5)
	p.Note("n")
	p.Warn("w")
	p.Meter("m", Bytes).Add(1)
	p.Done(nil)
	Tally(bg, "read", Bytes).Add(1)
	Hint(bg, "h")
	Plan(bg, "a")
	PlanSteps(bg, Step{Name: "a", Unit: Bytes, Total: 1})
	Unplan(bg, "a")
	p.Item("i")
	Skip(bg, "a", "why")
	if Within(bg, nil) != bg {
		t.Fatal("Within(nil) changed the context")
	}
	var r *Reporter
	r.Finish(nil)
}

func TestPlanStartSkip(t *testing.T) {
	ctx, r, _ := newTest(t)
	Plan(ctx, "store", "index", "full-text")
	Skip(ctx, "full-text", "no --fts")
	s := Start(ctx, "store", Bytes, 100)
	extra := Start(ctx, "compact", None, 0)
	snap := r.Snapshot(0)
	if len(snap.Phases) != 4 {
		t.Fatalf("%d phases, want 4: %+v", len(snap.Phases), snap.Phases)
	}
	// compact was not planned: it goes before the pending index.
	want := []struct {
		name  string
		state State
	}{{"store", Running}, {"compact", Running}, {"index", Pending}, {"full-text", Skipped}}
	for i, w := range want {
		if v := snap.Phases[i]; v.Name != w.name || v.State != w.state {
			t.Errorf("phase %d = %s/%d, want %s/%d", i, v.Name, v.State, w.name, w.state)
		}
	}
	if snap.Phases[3].Reason != "no --fts" {
		t.Errorf("skip reason %q", snap.Phases[3].Reason)
	}
	if s.id != snap.Phases[0].ID || extra.id != snap.Phases[1].ID {
		t.Error("Start did not reuse the planned phase")
	}

	// A phase nobody planned goes before the pending ones.
	ctx, r, _ = newTest(t)
	Plan(ctx, "store", "index", "graph")
	Start(ctx, "store", Bytes, 1).Done(nil)
	Start(ctx, "partition hashes", Bytes, 1)
	var names []string
	for _, v := range r.Snapshot(0).Phases {
		names = append(names, v.Name)
	}
	if got := fmt.Sprint(names); got != "[store partition hashes index graph]" {
		t.Errorf("order %s", got)
	}
}

// PlanSteps gives pending phases the work they expect; Start replaces it
// with the real figures. Only planned phases say so.
func TestPlanSteps(t *testing.T) {
	ctx, r, _ := newTest(t)
	PlanSteps(ctx, Step{"identify", Bytes, 100}, Step{"store", Bytes, 100}, Step{"index", Files, Unknown})
	Plan(ctx, "full-text")
	vs := r.Snapshot(0).Phases
	if len(vs) != 4 {
		t.Fatalf("%d phases: %+v", len(vs), vs)
	}
	for i, w := range []struct {
		unit  Unit
		total int64
	}{{Bytes, 100}, {Bytes, 100}, {Files, Unknown}, {None, Unknown}} {
		if v := vs[i]; v.State != Pending || !v.Planned || v.Unit != w.unit || v.Total != w.total {
			t.Errorf("%s: %+v, want pending planned %q of %d", v.Name, v, w.unit, w.total)
		}
	}

	// A later plan updates a step still pending, never one that started.
	Start(ctx, "identify", Bytes, 120)
	PlanSteps(ctx, Step{"identify", Bytes, 5}, Step{"store", Bytes, 90})
	extra := Start(ctx, "partition hashes", Bytes, 10)
	vs = r.Snapshot(0).Phases
	if vs[0].Total != 120 || vs[0].State != Running || !vs[0].Planned {
		t.Errorf("started identify: %+v", vs[0])
	}
	if vs[2].Name != "store" || vs[2].Total != 90 {
		t.Errorf("pending store: %+v", vs[2])
	}
	if vs[1].ID != extra.id || vs[1].Planned {
		t.Errorf("an unplanned phase says it was planned: %+v", vs[1])
	}
}

// Unplan drops pending phases without a trace: no event, no "not run".
func TestUnplan(t *testing.T) {
	ctx, r, _ := newTest(t)
	Plan(ctx, "identify", "store", "index")
	Start(ctx, "identify", Bytes, 10).Done(nil)
	seq := r.Snapshot(0).Seq
	Unplan(ctx, "identify", "store", "index", "graph")
	r.Finish(nil)
	snap := r.Snapshot(seq)
	if len(snap.Phases) != 1 || snap.Phases[0].Name != "identify" || snap.Phases[0].State != OK {
		t.Fatalf("phases after Unplan: %+v", snap.Phases)
	}
	if len(snap.Events) != 0 {
		t.Errorf("Unplan made events: %+v", snap.Events)
	}

	// Under a group, only the group's pending kids go.
	ctx, r, _ = newTest(t)
	Plan(ctx, "compact")
	group := Start(ctx, "evidence", Evidence, 1)
	Plan(Within(ctx, group), "store", "index")
	Unplan(Within(ctx, group), "store", "compact")
	vs := r.Snapshot(0).Phases
	if len(vs) != 2 || vs[1].Name != "compact" || len(vs[0].Kids) != 1 || vs[0].Kids[0].Name != "index" {
		t.Errorf("Unplan under a group: %+v", vs)
	}
}

func TestItem(t *testing.T) {
	ctx, r, _ := newTest(t)
	p := Start(ctx, "index", Files, Unknown)
	p.Item(`Windows\System32\config\SOFTWARE`)
	if v := r.Snapshot(0).Phases[0]; v.Item != `Windows\System32\config\SOFTWARE` {
		t.Errorf("item %q", v.Item)
	}
	p.Item("")
	if v := r.Snapshot(0).Phases[0]; v.Item != "" {
		t.Errorf("item not cleared: %q", v.Item)
	}
}

func TestWithinCurrentWithout(t *testing.T) {
	ctx, r, _ := newTest(t)
	group := Start(ctx, "evidence", Evidence, 2)
	inner := Within(ctx, group)
	if Current(inner) != group {
		t.Fatal("Current is not the group")
	}
	child := Start(inner, "store", Bytes, 10)
	child.Add(10)
	child.Done(nil)
	if kids := r.Snapshot(0).Phases[0].Kids; len(kids) != 1 || kids[0].Name != "store" {
		t.Fatalf("child not under the group: %+v", kids)
	}
	if CurrentIn(inner, Evidence) != group || CurrentIn(inner, Bytes) != nil {
		t.Fatal("CurrentIn did not check the unit")
	}
	quiet := Without(inner)
	if From(quiet) != nil || Current(quiet) != nil {
		t.Fatal("Without left a reporter or phase visible")
	}
	if Start(quiet, "x", None, 0) != nil {
		t.Fatal("Start under Without made a phase")
	}
	if Without(context.Background()) == nil {
		t.Fatal("Without on a plain context")
	}
}

func TestDoneClassification(t *testing.T) {
	warning := &lifecycle.WarningError{Err: errors.New("index partial")}
	cases := []struct {
		name   string
		warn   string
		err    error
		state  State
		reason string
		warned string
	}{
		{"ok", "", nil, OK, "", ""},
		{"warned", "3 files unreadable", nil, Warning, "", "3 files unreadable"},
		{"warning error", "", fmt.Errorf("store: %w", warning), Warning, "", "store: index partial"},
		{"signal", "", &lifecycle.SignalError{Signal: os.Interrupt}, Interrupted, (&lifecycle.SignalError{Signal: os.Interrupt}).Error(), ""},
		{"bare cancel, command alive", "", context.Canceled, Failed, "context canceled", ""},
		{"failure", "", errors.New("disk full"), Failed, "disk full", ""},
	}
	for _, c := range cases {
		ctx, r, _ := newTest(t)
		p := Start(ctx, "store", Bytes, 100)
		p.Add(63)
		if c.warn != "" {
			p.Warn(c.warn)
		}
		p.Done(c.err)
		p.Done(nil) // the first call wins
		v := r.Snapshot(0).Phases[0]
		if v.State != c.state || v.Reason != c.reason || v.Warn != c.warned {
			t.Errorf("%s: state %d reason %q warn %q; want %d %q %q", c.name, v.State, v.Reason, v.Warn, c.state, c.reason, c.warned)
		}
		if v.Done != 63 {
			t.Errorf("%s: Done changed the count to %d", c.name, v.Done)
		}
	}
}

func TestInterruptedWhenCommandCancelled(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	r := New(root, nil)
	ctx := With(root, r)
	p := Start(ctx, "store", Bytes, 10)
	cancel()
	p.Done(context.Canceled)
	if v := r.Snapshot(0).Phases[0]; v.State != Interrupted {
		t.Fatalf("state %d, want interrupted", v.State)
	}
}

func TestFinish(t *testing.T) {
	ctx, r, _ := newTest(t)
	Plan(ctx, "store", "index")
	Start(ctx, "store", Bytes, 10)
	r.Finish(errors.New("boom"))
	r.Finish(nil) // once only
	snap := r.Snapshot(0)
	if snap.Phases[0].State != Failed || snap.Phases[0].Reason != "boom" {
		t.Errorf("running phase after Finish: %+v", snap.Phases[0])
	}
	if snap.Phases[1].State != Skipped || snap.Phases[1].Reason != "not run" {
		t.Errorf("pending phase after Finish: %+v", snap.Phases[1])
	}
	if !snap.Finished || snap.Outcome != Failed {
		t.Errorf("outcome %d finished %v", snap.Outcome, snap.Finished)
	}

	ctx, r, _ = newTest(t)
	p := Start(ctx, "store", Bytes, 10)
	p.Warn("x")
	p.Done(nil)
	r.Finish(nil)
	if r.Snapshot(0).Outcome != Warning {
		t.Error("a warned phase did not make the outcome a warning")
	}
}

func TestEventsAndRing(t *testing.T) {
	ctx, r, _ := newTest(t)
	g := Start(ctx, "evidence", Evidence, 2)
	in := Within(ctx, g)
	Start(in, "a.dd", None, 0).Done(nil)
	snap := r.Snapshot(0)
	if len(snap.Events) != 3 {
		t.Fatalf("%d events, want 3", len(snap.Events))
	}
	if e := snap.Events[2]; !e.Ended || e.Phase.Name != "a.dd" || len(e.Path) != 1 || e.Path[0] != "evidence" {
		t.Fatalf("end event %+v", e)
	}
	if n := len(r.Snapshot(snap.Seq).Events); n != 0 {
		t.Fatalf("%d events after the last seen", n)
	}
	for i := range eventCap + 10 {
		Start(in, fmt.Sprint(i), None, 0).Done(nil)
	}
	snap = r.Snapshot(0)
	if len(snap.Events) != eventCap || snap.Dropped == 0 {
		t.Fatalf("ring holds %d events, dropped %d", len(snap.Events), snap.Dropped)
	}
	r.Prune()
	g.Done(nil)
}

func TestTalliesHintsMeters(t *testing.T) {
	ctx, r, _ := newTest(t)
	Tally(ctx, "read", Bytes).Add(100)
	Tally(ctx, "read", Bytes).Add(20)
	Hint(ctx, "rerun to resume")
	Hint(ctx, "rerun to resume")
	p := Start(ctx, "store", Bytes, 10)
	p.Meter("new", Bytes).Add(7)
	p.Meter("new", Bytes).Add(1)
	p.Note("p2 ntfs")
	snap := r.Snapshot(0)
	if len(snap.Tallies) != 1 || snap.Tallies[0].Value != 120 {
		t.Errorf("tallies %+v", snap.Tallies)
	}
	if len(snap.Hints) != 1 {
		t.Errorf("hints %q", snap.Hints)
	}
	if m := snap.Phases[0].Meters; len(m) != 1 || m[0].Value != 8 || snap.Phases[0].Note != "p2 ntfs" {
		t.Errorf("meters %+v note %q", m, snap.Phases[0].Note)
	}
}

func TestPruneFoldsFinishedGroups(t *testing.T) {
	ctx, r, _ := newTest(t)
	g := Start(ctx, "a.dd", None, 0)
	Start(Within(ctx, g), "store", Bytes, 1).Done(nil)
	g.Done(nil)
	if kids := r.Snapshot(0).Phases[0].Kids; len(kids) != 0 {
		t.Fatalf("a finished group shows %d kids", len(kids))
	}
	r.Prune()
	if len(r.phases[0].kids) != 0 {
		t.Fatal("Prune kept a finished group's phases")
	}
}

// TestConcurrentAdds is meant for -race: many goroutines count while
// phases start and end and a renderer takes snapshots.
func TestConcurrentAdds(t *testing.T) {
	ctx, r, _ := newTest(t)
	p := Start(ctx, "store", Bytes, Unknown)
	m := p.Meter("new", Bytes)
	read := Tally(ctx, "read", Bytes)
	const workers, adds = 64, 10000
	stop := make(chan struct{})
	var last int64
	var snapErr error
	var watcher sync.WaitGroup
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			d := r.Snapshot(0).Phases[0].Done
			if d < last {
				snapErr = fmt.Errorf("count went back from %d to %d", last, d)
				return
			}
			last = d
		}
	}()
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range adds {
				p.Add(1)
				m.Add(2)
				read.Add(3)
				if i%1000 == 0 {
					Start(Within(ctx, p), fmt.Sprintf("w%d-%d", w, i), None, 0).Done(nil)
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	watcher.Wait()
	if snapErr != nil {
		t.Fatal(snapErr)
	}
	snap := r.Snapshot(0)
	if got := snap.Phases[0].Done; got != workers*adds {
		t.Fatalf("done %d, want %d", got, workers*adds)
	}
	if got := snap.Phases[0].Meters[0].Value; got != 2*workers*adds {
		t.Fatalf("meter %d", got)
	}
	if got := snap.Tallies[0].Value; got != 3*workers*adds {
		t.Fatalf("tally %d", got)
	}
}

func BenchmarkAdd(b *testing.B) {
	r := New(context.Background(), nil)
	p := Start(With(context.Background(), r), "store", Bytes, Unknown)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p.Add(1)
		}
	})
}

func BenchmarkAddNoReporter(b *testing.B) {
	var p *Phase
	b.ReportAllocs()
	for b.Loop() {
		p.Add(1)
	}
}
