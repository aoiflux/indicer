package testutil

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	os.Exit(RunChildScenarios(m, map[string]func(args []string) int{
		"exit3": func([]string) int { return 3 },
		// args prints its arguments, one per line.
		"args": func(args []string) int {
			for _, a := range args {
				fmt.Println(a)
			}
			return 0
		},
		// point reports a fault point, then waits for an interrupt and exits
		// 130, as DUES commands will.
		"point": func([]string) int {
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt)
			if err := NotifyPoint("ready"); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 98
			}
			select {
			case <-sig:
				return 130
			case <-time.After(30 * time.Second):
				return 97
			}
		},
	}))
}

// TestIsolatedArgs: a binary a test runs reads no config file and writes
// its log in a directory of the test's own, unless the test chooses a config
// file or a log directory itself.
func TestIsolatedArgs(t *testing.T) {
	const logs = "<logs>"
	for _, c := range []struct{ args, want []string }{
		{nil, []string{"--no-config", "--log-dir", logs}},
		{[]string{"list", "--dbpath", "db"}, []string{"--no-config", "--log-dir", logs, "list", "--dbpath", "db"}},
		{[]string{"--config", "c.json", "list"}, []string{"--config", "c.json", "list"}},
		{[]string{"list", "--config=c.json"}, []string{"list", "--config=c.json"}},
		{[]string{"--no-config", "list"}, []string{"--log-dir", logs, "--no-config", "list"}},
		{[]string{"--log-dir", "L", "list"}, []string{"--no-config", "--log-dir", "L", "list"}},
		{[]string{"list", "--log-dir=L"}, []string{"--no-config", "list", "--log-dir=L"}},
		{[]string{"store", "--", "--config", "--log-dir"}, []string{"--no-config", "--log-dir", logs, "store", "--", "--config", "--log-dir"}},
	} {
		got := IsolatedArgs(t, slices.Clone(c.args))
		for i := 1; i < len(got); i++ {
			if got[i-1] == "--log-dir" && filepath.IsAbs(got[i]) {
				if fi, err := os.Stat(got[i]); err != nil || !fi.IsDir() {
					t.Errorf("IsolatedArgs(%q): log directory %s: %v", c.args, got[i], err)
				}
				got[i] = logs
			}
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("IsolatedArgs(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

// TestChildArgs: a child gets its arguments as given, flag-like ones
// included, and not its parent's scenario flags.
func TestChildArgs(t *testing.T) {
	args := []string{"a dir", "-dues.child=exit3", "--faults=x", ""}
	c := StartChild(t, "args", args...)
	if code := c.Wait(t, 30*time.Second); code != 0 {
		t.Fatalf("exit code %d\n%s", code, c.Output())
	}
	if got, want := c.Output(), strings.Join(args, "\n")+"\n"; got != want {
		t.Fatalf("child printed %q, want %q", got, want)
	}
}

func TestChildExitCode(t *testing.T) {
	c := StartChild(t, "exit3")
	if got := c.Wait(t, 30*time.Second); got != 3 {
		t.Fatalf("exit code %d, want 3\n%s", got, c.Output())
	}
}

func TestChildInterrupt(t *testing.T) {
	c := StartChild(t, "point")
	c.WaitForPoint(t, "ready", 30*time.Second)
	c.Interrupt(t)
	if got := c.Wait(t, 30*time.Second); got != 130 {
		t.Fatalf("exit code %d, want 130\n%s", got, c.Output())
	}
}

func TestChildKill(t *testing.T) {
	c := StartChild(t, "point")
	c.WaitForPoint(t, "ready", 30*time.Second)
	c.Kill(t)
}

func TestFailingReaderAt(t *testing.T) {
	src := bytes.NewReader(RandomBytes(1, 100))
	r := &FailingReaderAt{R: src, FailAt: 50}
	buf := make([]byte, 40)
	if n, err := r.ReadAt(buf, 0); n != 40 || err != nil {
		t.Fatalf("read before fault: n=%d err=%v", n, err)
	}
	n, err := r.ReadAt(buf, 30)
	if n != 20 || !errors.Is(err, ErrInjected) {
		t.Fatalf("read across fault: n=%d err=%v, want 20 and ErrInjected", n, err)
	}
	if _, err := r.ReadAt(buf, 60); !errors.Is(err, ErrInjected) {
		t.Fatalf("read past fault: %v", err)
	}
}

func TestShortReaderAt(t *testing.T) {
	r := &ShortReaderAt{R: bytes.NewReader(make([]byte, 100)), Max: 7}
	n, err := r.ReadAt(make([]byte, 50), 0)
	if n != 7 || err != nil {
		t.Fatalf("n=%d err=%v, want 7 nil", n, err)
	}
}

func TestENOSPCWriter(t *testing.T) {
	var sink bytes.Buffer
	w := NewENOSPCWriter(&sink, 10)
	if n, err := w.Write([]byte("12345")); n != 5 || err != nil {
		t.Fatalf("first write n=%d err=%v", n, err)
	}
	n, err := w.Write([]byte("67890abc"))
	if n != 5 || !errors.Is(err, ErrNoSpace()) {
		t.Fatalf("crossing write n=%d err=%v, want 5 and disk full", n, err)
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, ErrNoSpace()) {
		t.Fatalf("write when full: %v", err)
	}
	if sink.String() != "1234567890" || w.Written() != 10 {
		t.Fatalf("sink %q written %d", sink.String(), w.Written())
	}
}

func TestShortWriterAndErrCloser(t *testing.T) {
	var sink bytes.Buffer
	sw := &ShortWriter{W: &sink, Max: 3}
	if n, _ := sw.Write([]byte("abcdef")); n != 3 {
		t.Fatalf("short write n=%d", n)
	}
	ec := &ErrCloser{Inner: io.NopCloser(strings.NewReader("hi"))}
	if err := ec.Close(); !errors.Is(err, ErrInjected) || ec.Closes() != 1 {
		t.Fatalf("close err=%v closes=%d", err, ec.Closes())
	}
}

func TestRandomBytesDeterministic(t *testing.T) {
	a, b := RandomBytes(42, 1001), RandomBytes(42, 1001)
	if !bytes.Equal(a, b) {
		t.Fatal("same seed gave different bytes")
	}
	if bytes.Equal(a, RandomBytes(43, 1001)) {
		t.Fatal("different seeds gave the same bytes")
	}
	// Pin the stream so a change to the generator is noticed: fixtures and
	// goldens are built from it.
	const pinned = "c091aa16c1be83959e8ec8986b4a235574047aea747396ba6ed90fb8c487b587"
	if got := BytesSHA256(RandomBytes(1, 4096)); got != pinned {
		t.Fatalf("RandomBytes(1, 4096) digest %s, pinned %s", got, pinned)
	}
}

func TestTreeHashAndAssertNoFiles(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, d := range []string{a, b} {
		WriteRandomFile(t, d, "x/one.bin", 1, 100)
		WriteRandomFile(t, d, "two.bin", 2, 50)
	}
	if TreeHash(t, a) != TreeHash(t, b) {
		t.Fatal("equal trees hash differently")
	}
	WriteRandomFile(t, b, "two.bin", 3, 50)
	if TreeHash(t, a) == TreeHash(t, b) {
		t.Fatal("different trees hash equal")
	}
	AssertNoFilesMatching(t, a, "*.tmp")
	if got := FilesMatching(t, a, "*.bin"); len(got) != 2 {
		t.Fatalf("FilesMatching = %v", got)
	}
}

func TestFakeClock(t *testing.T) {
	c := NewFakeClock(time.Unix(0, 0))
	ch := c.After(time.Second)
	c.Advance(500 * time.Millisecond)
	select {
	case <-ch:
		t.Fatal("fired early")
	default:
	}
	c.Advance(500 * time.Millisecond)
	select {
	case <-ch:
	default:
		t.Fatal("did not fire")
	}
	if c.Waiters() != 0 {
		t.Fatal("waiter not removed")
	}
}

func TestPeakHeapAndAllocBelow(t *testing.T) {
	var keep []byte
	peak := PeakHeap(func() { keep = make([]byte, 8<<20); time.Sleep(5 * time.Millisecond) })
	if peak < 8<<20 {
		t.Fatalf("peak %d below the 8 MiB allocation", peak)
	}
	_ = keep
	AllocBelow(t, 1<<20, func() { _ = make([]byte, 1024) })
}

var testDir = NewDirFlag("dues.test-dir", "a directory for TestDirFlag")

func TestDirFlag(t *testing.T) {
	set := func(v string) {
		t.Helper()
		if err := flag.Set("dues.test-dir", v); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { set("") })

	ft := &fakeTB{TB: t}
	testDir.Require(ft)
	if !ft.skipped {
		t.Fatal("no skip without the flag")
	}

	dir := t.TempDir()
	set(dir)
	if got := testDir.Require(t); got != dir {
		t.Fatalf("got %q, want %q", got, dir)
	}

	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{filepath.Join(dir, "missing"), filepath.Join(dir, "file")} {
		set(bad)
		ft := &fakeTB{TB: t}
		testDir.Require(ft)
		if !ft.failed || ft.skipped {
			t.Errorf("-dues.test-dir=%s: failed %t, skipped %t; want a failure", bad, ft.failed, ft.skipped)
		}
	}
}

// fakeTB records failures and cleanups instead of acting on them, so a
// helper's failure path can be tested.
type fakeTB struct {
	testing.TB
	failed, skipped bool
	cleanups        []func()
}

func (f *fakeTB) Helper()               {}
func (f *fakeTB) Cleanup(fn func())     { f.cleanups = append(f.cleanups, fn) }
func (f *fakeTB) Errorf(string, ...any) { f.failed = true }
func (f *fakeTB) Fatalf(string, ...any) { f.failed = true }
func (f *fakeTB) Skipf(string, ...any)  { f.skipped = true }
