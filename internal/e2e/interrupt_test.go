//go:build faultinject

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"indicer/internal/faultinject"
	"indicer/internal/testutil"
)

// The interrupt tests send each command the signal an examiner sends with
// Ctrl-C (Ctrl-Break on Windows), and on Unix SIGTERM, while it works. The
// command must stop with the signal's exit code, leave nothing half written,
// release the database, and leave it recoverable to what an uninterrupted
// run gives.

type signalCase struct {
	name string // the faultinject signal action's argument
	code int
}

// signals are the signals the platform can send a command at a point.
func signals() []signalCase {
	s := []signalCase{{"INT", 130}}
	if runtime.GOOS != "windows" {
		s = append(s, signalCase{"TERM", 143})
	}
	return s
}

// interrupt runs dues with args, signalled at the at'th hit of point, and
// checks it exits with the signal's code.
func (l *crashLab) interrupt(sig signalCase, point, at string, args ...string) string {
	l.t.Helper()
	return interruptBinary(l.t, l.bin, l.work, sig, point, at, append(append(args, "--dbpath", l.root, "-c", "64"), l.layout...)...)
}

func interruptBinary(t *testing.T, bin, work string, sig signalCase, point, at string, args ...string) string {
	t.Helper()
	c := testutil.StartDues(t, bin, work, spec(point, "signal:"+sig.name+at), args...)
	code := c.Wait(t, 2*time.Minute)
	b, _ := os.ReadFile(c.Notify)
	if !strings.Contains(string(b), point) {
		t.Fatalf("%s: never reached %s (exit %d)\n%s", strings.Join(args, " "), point, code, c.Output())
	}
	if code != sig.code {
		t.Fatalf("%s interrupted at %s: exit %d, want %d\n%s", strings.Join(args, " "), point, code, sig.code, c.Output())
	}
	return c.Output()
}

// noFiles fails if any of the paths exists.
func noFiles(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("an interrupted command left %s", p)
		}
		dir := filepath.Dir(p)
		if left := testutil.FilesMatching(t, dir, "."+filepath.Base(p)+".dues-tmp-*"); len(left) > 0 {
			t.Fatalf("an interrupted command left %v", left)
		}
	}
}

// TestInterruptedWrites interrupts the commands that write to the database.
// Each must leave it recoverable to what an uninterrupted run leaves.
func TestInterruptedWrites(t *testing.T) {
	ref := reference(t)
	cases := []struct {
		name  string
		point string
		at    string
		setup func(l *crashLab)
		args  func(l *crashLab) []string
	}{
		{
			name: "store", point: faultinject.IngestAfterPending,
			setup: func(l *crashLab) { l.must("store", l.files[1]) },
			args:  func(l *crashLab) []string { return []string{"store", l.files[0]} },
		},
		{
			name: "store-indexing", point: faultinject.IndexMid, at: "@2",
			setup: func(l *crashLab) { l.must("store", l.files[0]) },
			args:  func(l *crashLab) []string { return []string{"store", l.files[1]} },
		},
		{
			name: "repair", point: faultinject.RecoveryMid,
			setup: func(l *crashLab) {
				l.must("store", l.files[0])
				l.crash(spec(faultinject.IngestBeforeCommit, "exit:3"), "store", l.files[1])
			},
			args: func(*crashLab) []string { return []string{"repair", "--fix"} },
		},
		{
			name: "reindex", point: faultinject.IndexMid, at: "@2",
			setup: func(l *crashLab) { l.storeAll() },
			args:  func(*crashLab) []string { return []string{"reindex", "--all"} },
		},
	}
	for _, c := range cases {
		for _, sig := range signals() {
			t.Run(c.name+"/"+sig.name, func(t *testing.T) {
				l := newCrashLab(t)
				c.setup(l)
				l.interrupt(sig, c.point, c.at, c.args(l)...)
				l.recoverAndCheck(ref)
			})
		}
	}
}

// TestInterruptedReads interrupts the commands that read the database (and
// write a report or a file): they write nothing, and leave the database as
// it was and free.
func TestInterruptedReads(t *testing.T) {
	// near needs reverse relations.
	revrel := func() *crashLab {
		l := newCrashLab(t)
		l.storeFlags = []string{"--revrel"}
		l.storeAll()
		return l
	}
	ref := revrel().list()
	for _, sig := range signals() {
		t.Run(sig.name, func(t *testing.T) {
			l := revrel()
			hashes := l.list().hashes
			out := filepath.Join(l.work, "restored.bin")
			report := filepath.Join(l.work, "report.json")
			nearReport := filepath.Join(l.work, "near.json")

			l.interrupt(sig, faultinject.RestoreMid, "@1", "restore", hashes[0], "-f", out)
			l.interrupt(sig, faultinject.SearchMid, "", "search", "DUES", "--out", report)
			l.interrupt(sig, faultinject.NearMid, "", "near", "in", hashes[0], "--out", nearReport)
			noFiles(t, out, report, nearReport)

			if got := l.list(); strings.Join(got.lines, "\n") != strings.Join(ref.lines, "\n") {
				t.Fatalf("listing changed:\n%s", strings.Join(got.lines, "\n"))
			}
			l.checkStored(l.list())
			l.noLeftovers()
		})
	}
}

// TestInterruptedGraphCommands interrupts enrich and micro extract, which
// write the graph: the database is untouched and free, and a second run
// finishes.
func TestInterruptedGraphCommands(t *testing.T) {
	for _, sig := range signals() {
		t.Run(sig.name, func(t *testing.T) {
			l := newCrashLab(t)
			l.storeAll()
			l.interrupt(sig, faultinject.EnrichMid, "", "enrich")
			l.must("enrich")
			html := filepath.Join(l.work, "graph.html")
			l.interrupt(sig, faultinject.MicroMid, "@2", "micro", "extract", "--out", html)
			noFiles(t, html)
			l.must("micro", "extract", "--out", html)
			l.checkStored(l.list())
			l.noLeftovers()
		})
	}
}

// TestInterruptedServer signals the server while it finalizes an upload: it
// stops with the signal's code, removes the upload's part file, and leaves
// the database free and recoverable.
func TestInterruptedServer(t *testing.T) {
	ref := reference(t)
	for _, sig := range signals() {
		t.Run(sig.name, func(t *testing.T) {
			l := newCrashLab(t)
			l.must("store", l.files[1])
			addr := freeAddr(t)
			srv := testutil.StartDues(t, l.bin, l.work, spec(faultinject.UploadBeforeIngest, "signal:"+sig.name), "server", "--listen", addr, "--dbpath", l.root, "-c", "64")
			waitHealthy(t, addr, srv)
			// A draining server lets the request in flight finish, so the
			// upload may or may not be stored.
			_ = upload(addr, l.files[0])
			if code := srv.Wait(t, 2*time.Minute); code != sig.code {
				t.Fatalf("server: exit %d, want %d\n%s", code, sig.code, srv.Output())
			}
			if parts := testutil.FilesMatching(t, l.root, "*.part"); len(parts) > 0 {
				t.Fatalf("the server stopped and left %v", parts)
			}
			l.recoverAndCheck(ref)
		})
	}
}

// TestInterruptedArchiveCommands interrupts the compression command while it
// stores an archive's files and while it restores them.
func TestInterruptedArchiveCommands(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the compression binary")
	}
	bin := testutil.BuildBinary(t, "./cmd/compression", "faultinject")
	for _, sig := range signals() {
		t.Run(sig.name, func(t *testing.T) {
			work := t.TempDir()
			src := filepath.Join(work, "case")
			testutil.WriteRandomFile(t, src, "a.bin", 1, 300<<10)
			testutil.WriteRandomFile(t, filepath.Join(src, "sub"), "b.bin", 2, 5000)
			archive := filepath.Join(work, "case.duesarc")

			interruptBinary(t, bin, work, sig, faultinject.IngestAfterPending, "", "create-archive", "--input", src, "--output", archive)
			noFiles(t, archive)
			if r := testutil.RunDues(t, bin, work, 2*time.Minute, "create-archive", "--input", src, "--output", archive); r.ExitCode != 0 {
				t.Fatalf("create-archive: exit %d\n%s", r.ExitCode, r.Combined())
			}

			dst := filepath.Join(work, "extracted")
			interruptBinary(t, bin, work, sig, faultinject.RestoreMid, "@1", "extract-archive", "--input", archive, "--output", dst)
			if entries, err := os.ReadDir(dst); err == nil && len(entries) > 0 {
				t.Fatalf("an interrupted extraction left %d entries in its output", len(entries))
			}
			if r := testutil.RunDues(t, bin, work, 2*time.Minute, "extract-archive", "--input", archive, "--output", dst); r.ExitCode != 0 {
				t.Fatalf("extract-archive: exit %d\n%s", r.ExitCode, r.Combined())
			}
			if testutil.TreeHash(t, dst) != testutil.TreeHash(t, src) {
				t.Fatal("the archive does not extract to its input")
			}
		})
	}
}

// TestConcurrentProcesses: read commands run side by side; while a writer
// holds the database, every other command is refused with an error naming
// the cause, and the writer finishes unharmed.
func TestConcurrentProcesses(t *testing.T) {
	l := newCrashLab(t)
	l.storeAll()
	hashes := l.list().hashes

	reads := [][]string{{"list"}, {"stats"}, {"repair"}, {"search", "DUES", "--out", filepath.Join(l.work, "r1.json")},
		{"search", "abc", "--out", filepath.Join(l.work, "r2.json")}}
	for i, h := range hashes {
		reads = append(reads, []string{"restore", h, "-f", filepath.Join(l.work, fmt.Sprintf("p%d.bin", i))})
	}
	errs := make(chan error, len(reads))
	for _, args := range reads {
		go func() {
			r := l.dues("", args...)
			if r.ExitCode != 0 {
				errs <- fmt.Errorf("dues %s beside other readers: exit %d\n%s", strings.Join(args, " "), r.ExitCode, r.Combined())
				return
			}
			errs <- nil
		}()
	}
	for range reads {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	if t.Failed() {
		return
	}

	big := testutil.WriteRandomFile(t, l.work, "big.bin", 5, 4<<20)
	l.sums[testutil.FileSHA256(t, big)] = true
	w := testutil.StartDues(t, l.bin, l.work, spec(faultinject.IngestAfterPending, "sleep:4s"),
		"store", big, "--dbpath", l.root, "-c", "64")
	w.WaitForPoint(t, faultinject.IngestAfterPending, time.Minute)
	for _, args := range [][]string{{"store", l.files[0]}, {"list"}, {"repair", "--fix"}, {"reset", "--yes"}} {
		failsCleanly(t, l.dues("", args...), "in use by another DUES process", args...)
	}
	if code := w.Wait(t, 2*time.Minute); code != 0 {
		t.Fatalf("the writer: exit %d\n%s", code, w.Output())
	}
	got := l.list()
	if st := got.states["big.bin"]; st != "completed" {
		t.Fatalf("the writer's evidence is %q", st)
	}
	l.checkStored(got)
	l.noLeftovers()
}
