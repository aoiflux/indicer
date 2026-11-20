package safe

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func withReporter(t *testing.T, r Reporter) {
	t.Helper()
	prevStrict := SetStrict(false)
	prev := SetReporter(r)
	t.Cleanup(func() { SetReporter(prev); SetStrict(prevStrict) })
}

func TestCallRecoversPanic(t *testing.T) {
	var firsts []bool
	withReporter(t, func(e *PanicError, first bool) { firsts = append(firsts, first) })

	site := Site{Component: "test/call", Op: "walk", Path: "/a/b", Inode: 7}
	for i := 0; i < 3; i++ {
		err := Call(site, func() error {
			none := []int{}
			_ = none[i] // an out-of-range index is a runtime panic
			return nil
		})
		var pe *PanicError
		if !errors.As(err, &pe) {
			t.Fatalf("Call returned %v, want *PanicError", err)
		}
		if pe.Site.Path != "/a/b" || pe.ID == "" || len(pe.Stack) == 0 {
			t.Fatalf("panic error is missing its site, id or stack: %+v", pe)
		}
		if !strings.Contains(err.Error(), "test/call/walk") || !strings.Contains(err.Error(), pe.ID) {
			t.Fatalf("message does not name the site and id: %v", err)
		}
	}
	if len(firsts) != 3 || !firsts[0] || firsts[1] || firsts[2] {
		t.Fatalf("reporter first flags %v, want [true false false]", firsts)
	}
	if got := Counts()["test/call/walk"]; got != 3 {
		t.Fatalf("count %d, want 3", got)
	}
}

func TestCallPassesErrorsThrough(t *testing.T) {
	t.Parallel()
	want := errors.New("plain")
	if err := Call(Site{Component: "test", Op: "plain"}, func() error { return want }); err != want {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func TestPanicErrorUnwrapsErrorValue(t *testing.T) {
	withReporter(t, nil)
	sentinel := errors.New("boom")
	err := Call(Site{Component: "test", Op: "unwrap"}, func() error { panic(sentinel) })
	if !errors.Is(err, sentinel) || !IsPanic(err) {
		t.Fatalf("got %v; want a panic error wrapping the sentinel", err)
	}
}

func TestValue(t *testing.T) {
	withReporter(t, nil)
	n, err := Value(Site{Component: "test", Op: "value"}, func() (int, error) {
		var s []int
		return s[3], nil
	})
	if n != 0 || !IsPanic(err) {
		t.Fatalf("Value = %d, %v; want 0 and a panic error", n, err)
	}
	n, err = Value(Site{Component: "test", Op: "value"}, func() (int, error) { return 5, nil })
	if n != 5 || err != nil {
		t.Fatalf("Value = %d, %v; want 5, nil", n, err)
	}
}

func TestStrictRepanics(t *testing.T) {
	withReporter(t, nil)
	SetStrict(true)
	defer func() {
		if recover() == nil {
			t.Fatal("strict mode did not re-panic")
		}
	}()
	_ = Call(Site{Component: "test", Op: "strict"}, func() error { panic("x") })
}

func TestGoDeliversExactlyOnce(t *testing.T) {
	withReporter(t, nil)
	ch := Go(Site{Component: "test", Op: "go"}, func() error { panic("in goroutine") })
	if err := <-ch; !IsPanic(err) {
		t.Fatalf("got %v, want a panic error", err)
	}
	if _, open := <-ch; open {
		t.Fatal("channel not closed after the single result")
	}
}

func TestGroupCancelsAndWaits(t *testing.T) {
	withReporter(t, nil)
	g, ctx := WithContext(context.Background())
	var finished atomic.Int32
	g.Go(Site{Component: "test", Op: "group"}, func() error { panic("first") })
	for i := 0; i < 4; i++ {
		g.Go(Site{Component: "test", Op: "group"}, func() error {
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
			}
			finished.Add(1)
			return ctx.Err()
		})
	}
	err := g.Wait()
	if !IsPanic(err) {
		t.Fatalf("Wait returned %v, want the panic error", err)
	}
	if n := finished.Load(); n != 4 {
		t.Fatalf("%d goroutines finished before Wait returned, want 4", n)
	}
	if !IsPanic(context.Cause(ctx)) {
		t.Fatalf("context cause %v, want the panic error", context.Cause(ctx))
	}
}

func TestGroupLimit(t *testing.T) {
	t.Parallel()
	g, _ := WithContext(context.Background())
	g.SetLimit(2)
	var running, peak atomic.Int32
	for i := 0; i < 10; i++ {
		g.Go(Site{Component: "test", Op: "limit"}, func() error {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			running.Add(-1)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	if p := peak.Load(); p > 2 {
		t.Fatalf("peak concurrency %d, limit 2", p)
	}
}

// TestStrictDefault: an ordinary build contains a panic; a safestrict
// build (the fuzz smoke) re-panics it, with no switch at run time.
func TestStrictDefault(t *testing.T) {
	prev := SetReporter(nil)
	t.Cleanup(func() { SetReporter(prev) })
	repanicked := true
	defer func() {
		if v := recover(); (v != nil) != strictDefault || repanicked != strictDefault {
			t.Fatalf("strictDefault %v: re-panicked %v, recovered %v", strictDefault, repanicked, v)
		}
	}()
	err := Call(Site{Component: "test", Op: "default"}, func() error { panic("x") })
	repanicked = false
	if !IsPanic(err) {
		t.Fatalf("got %v, want a panic error", err)
	}
}
