package diskspace

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func fakeFree(t *testing.T, free *uint64, calls *int) {
	t.Helper()
	prev := freeSpace
	freeSpace = func(string) (uint64, error) {
		*calls++
		return *free, nil
	}
	t.Cleanup(func() { freeSpace = prev })
}

func TestCheckKeepsTheReserve(t *testing.T) {
	var free uint64
	var calls int
	fakeFree(t, &free, &calls)

	free = Reserve + 10
	if err := Check("/db", 10); err != nil {
		t.Fatalf("exactly the reserve left: %v", err)
	}
	err := Check("/db", 11)
	var low *LowSpaceError
	if !errors.As(err, &low) || !errors.Is(err, ErrLow) || !IsNoSpace(err) {
		t.Fatalf("one byte into the reserve: %v", err)
	}
	if low.Free != Reserve+10 || low.Need != 11 || !strings.Contains(err.Error(), "/db") {
		t.Fatalf("error %+v: %v", low, err)
	}
	free = 5
	if err := Check("/db", 10); !errors.Is(err, ErrLow) {
		t.Fatalf("less free than the write: %v", err)
	}
}

// TestGuardChecksInSteps: a guard asks the filesystem once per checkBytes
// written, counts what it has not yet checked as still to be written, and
// keeps failing once the volume was low.
func TestGuardChecksInSteps(t *testing.T) {
	var free uint64 = Reserve + 40<<20
	var calls int
	fakeFree(t, &free, &calls)

	g := NewGuard("/db")
	g.last = time.Now() // as if just checked
	for range 15 {
		if err := g.Add(1 << 20); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 0 {
		t.Fatalf("checked %d times before %d bytes", calls, checkBytes)
	}
	if err := g.Add(1 << 20); err != nil || calls != 1 {
		t.Fatalf("at %d bytes: %v after %d checks", checkBytes, err, calls)
	}

	free = Reserve + 8<<20
	g.last = time.Now()
	var err error
	for i := 0; i < 16 && err == nil; i++ {
		err = g.Add(1 << 20)
	}
	if !errors.Is(err, ErrLow) {
		t.Fatalf("16 MiB pending with 8 MiB above the reserve: %v", err)
	}
	free = 1 << 40
	if err := g.Add(1); !errors.Is(err, ErrLow) {
		t.Fatalf("after reporting low space: %v", err)
	}
	if err := GuardFor("/other").Add(1); err != nil {
		t.Fatal(err)
	}
}

// TestGuardForStartsAgainAfterLowSpace: once a guard reported low space,
// the next writer to the volume gets a fresh guard, which checks again, so
// freeing space lets a long-running process (the TUI, the server) store
// again.
func TestGuardForStartsAgainAfterLowSpace(t *testing.T) {
	var free uint64
	var calls int
	fakeFree(t, &free, &calls)
	path := t.Name()
	g := GuardFor(path)
	if err := g.Add(checkBytes); !errors.Is(err, ErrLow) {
		t.Fatalf("no space: %v", err)
	}
	free = 1 << 40
	if err := GuardFor(path).Add(checkBytes); err != nil {
		t.Fatalf("after space was freed: %v", err)
	}
}
