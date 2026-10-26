//go:build faultinject

package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"indicer/internal/faultinject"
	"indicer/internal/testutil"
	"indicer/internal/testutil/fsfixture"

	"github.com/zeebo/blake3"
)

// The crash tests kill dues at each fault point, as a power loss or a killed
// process would, then check what the plan's index-integrity invariant 6
// requires: after recovery and a resumed run, the database holds exactly
// what an uninterrupted run leaves, every stored evidence restores verified,
// and nothing the dead process was writing is left behind.

// crashExit is the exit code the faults use. dues itself never exits 3, so
// it also proves the point was reached.
const crashExit = 3

// crashLab is a data root, the evidence the scenario stores, and the binary
// built with fault injection.
type crashLab struct {
	t      *testing.T
	bin    string
	work   string
	root   string
	layout []string // database flags every command gets, e.g. --container
	// storeFlags are flags every store of the lab's files gets.
	storeFlags []string
	files      []string // evidence, in the order a run stores it
	sums       map[string]bool
}

func newCrashLab(t *testing.T, layout ...string) *crashLab {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs the dues binary")
	}
	l := &crashLab{
		t:      t,
		bin:    testutil.BuildDues(t, "faultinject"),
		work:   t.TempDir(),
		layout: layout,
		sums:   map[string]bool{},
	}
	l.root = filepath.Join(l.work, "db")
	// 1 MiB and 13 bytes in 64 KiB chunks: 17 chunks, the last one short.
	l.add(testutil.WriteRandomFile(t, l.work, "blob.bin", 12, 1<<20+13))
	img := fsfixture.Open(t, fixtureNamed(t, "fat/fat16"))
	l.add(copyFile(t, img, filepath.Join(l.work, "fat16.img")))
	return l
}

func (l *crashLab) add(path string) {
	l.files = append(l.files, path)
	l.sums[testutil.FileSHA256(l.t, path)] = true
}

// dues runs a command against the lab's root; faults ("point=action") are set
// for this run only.
func (l *crashLab) dues(faults string, args ...string) testutil.Result {
	l.t.Helper()
	var env []string
	if faults != "" {
		env = []string{"DUES_FAULTS=" + faults}
	}
	args = append(append(args, "--dbpath", l.root, "-c", "64"), l.layout...)
	return testutil.RunDuesEnv(l.t, l.bin, l.work, 2*time.Minute, env, args...)
}

// must runs a command that has to succeed.
func (l *crashLab) must(args ...string) testutil.Result {
	l.t.Helper()
	r := l.dues("", args...)
	if r.ExitCode != 0 {
		l.t.Fatalf("dues %s: exit %d\n%s", strings.Join(args, " "), r.ExitCode, r.Combined())
	}
	return r
}

// crash runs a command that must die at the fault.
func (l *crashLab) crash(faults string, args ...string) {
	l.t.Helper()
	r := l.dues(faults, args...)
	if r.ExitCode != crashExit {
		l.t.Fatalf("dues %s with %s: exit %d, want %d (the fault never fired?)\n%s",
			strings.Join(args, " "), faults, r.ExitCode, crashExit, r.Combined())
	}
}

func (l *crashLab) storeAll() {
	l.t.Helper()
	for _, f := range l.files {
		l.must(append([]string{"store", f}, l.storeFlags...)...)
	}
}

// listing is what `dues list` shows, without IDs and evidence names: the
// first ingest of a content names it, so which of two paths of one content
// names the evidence depends on which committed first.
type listing struct {
	lines  []string
	states map[string]string // evidence name -> status
	hashes []string          // evidence hashes, for restore
}

func (l *crashLab) list() listing {
	l.t.Helper()
	r := l.must("list")
	i, j := strings.Index(r.Stdout, "{"), strings.LastIndex(r.Stdout, "}")
	if i < 0 || j < i {
		l.t.Fatalf("no JSON in list output:\n%s", r.Combined())
	}
	var doc struct {
		Evidence []struct {
			Name       string `json:"name"`
			Hash       string `json:"hash"`
			Size       int64  `json:"size"`
			Status     string `json:"status"`
			Partitions []struct {
				Hash         string `json:"hash"`
				FileName     string `json:"fileName"`
				Size         int64  `json:"size"`
				IndexedFiles []struct {
					Path      string `json:"path"`
					Hash      string `json:"hash"`
					Size      int64  `json:"size"`
					IsDeleted bool   `json:"isDeleted"`
				} `json:"indexedFiles"`
			} `json:"partitions"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(r.Stdout[i:j+1]), &doc); err != nil {
		l.t.Fatalf("list: %v\n%s", err, r.Stdout)
	}
	out := listing{states: map[string]string{}}
	for _, e := range doc.Evidence {
		out.states[e.Name] = e.Status
		out.lines = append(out.lines, fmt.Sprintf("E %s %d %s", e.Hash, e.Size, e.Status))
		if e.Status == "completed" {
			out.hashes = append(out.hashes, e.Hash)
		}
		for _, p := range e.Partitions {
			out.lines = append(out.lines, fmt.Sprintf("P %s %s %s %d", e.Hash, p.FileName, p.Hash, p.Size))
			for _, f := range p.IndexedFiles {
				out.lines = append(out.lines, fmt.Sprintf("F %s %s %q %s %d %v", e.Hash, p.FileName, f.Path, f.Hash, f.Size, f.IsDeleted))
			}
		}
	}
	sort.Strings(out.lines)
	return out
}

// reference is the listing an uninterrupted run of the lab's files leaves.
func reference(t *testing.T, layout ...string) listing {
	t.Helper()
	l := newCrashLab(t, layout...)
	l.storeAll()
	return l.list()
}

// recoverAndCheck finishes what the crash left (recovery, then the index
// passes it interrupted), runs the stores again, and checks the result
// against the reference.
func (l *crashLab) recoverAndCheck(ref listing) {
	l.t.Helper()
	l.must("repair", "--fix", "--reindex-failed")
	l.storeAll()
	got := l.list()
	if !slices.Equal(got.lines, ref.lines) {
		l.t.Fatalf("listing after recovery differs from an uninterrupted run\ngot:\n%s\nwant:\n%s",
			strings.Join(got.lines, "\n"), strings.Join(ref.lines, "\n"))
	}
	l.checkStored(got)
	l.noLeftovers()
}

// checkStored restores every completed evidence with full verification and
// checks its content is one of the inputs, then reads every chunk back.
func (l *crashLab) checkStored(got listing) {
	l.t.Helper()
	for i, h := range got.hashes {
		out := filepath.Join(l.work, fmt.Sprintf("restored-%d.bin", i))
		l.must("restore", h, "-f", out, "--verify", "full")
		if sum := testutil.FileSHA256(l.t, out); !l.sums[sum] {
			l.t.Fatalf("evidence %s restored to content no input has (%s)", h, sum)
		}
		if err := os.Remove(out); err != nil {
			l.t.Fatal(err)
		}
	}
	l.must("repair", "--verify")
}

// noLeftovers checks nothing a dead process was writing is left in the root.
func (l *crashLab) noLeftovers() {
	l.t.Helper()
	for _, pattern := range []string{"*.dues-tmp-*", "*.part"} {
		if left := testutil.FilesMatching(l.t, l.root, pattern); len(left) > 0 {
			l.t.Fatalf("left in the data root: %v", left)
		}
	}
}

func spec(point, rule string) string { return point + "=" + rule }

// TestCrashDuringIngest kills a store at every point of an ingest, then
// recovers and stores again.
func TestCrashDuringIngest(t *testing.T) {
	ref := reference(t)
	refContainer := reference(t, "--container")
	cases := []struct {
		point  string
		at     string // which hit fires
		file   int    // index into the lab's files
		layout []string
		want   []string // the attempt's status after recovery
	}{
		{point: faultinject.IngestAfterPending, file: 0, want: []string{"failed"}},
		{point: faultinject.StoreChunkBeforeBarrier, file: 0, want: []string{"failed"}},
		{point: faultinject.StoreChunkAfterBarrier, file: 0, want: []string{"failed"}},
		{point: faultinject.ContainerMidRecord, at: "@5", file: 0, layout: []string{"--container"}, want: []string{"failed"}},
		{point: faultinject.IngestBeforeFlushed, file: 0, want: []string{"failed"}},
		// The index pass runs beside the chunk pass, so either may be ahead.
		{point: faultinject.IndexMid, at: "@3", file: 1, want: []string{"failed", "completed"}},
		{point: faultinject.IngestBeforeCommit, file: 1, want: []string{"completed"}},
		{point: faultinject.IngestInCommit, file: 1, want: []string{"completed"}},
	}
	for _, c := range cases {
		t.Run(c.point, func(t *testing.T) {
			l := newCrashLab(t, c.layout...)
			want := ref
			if len(c.layout) > 0 {
				want = refContainer
			}
			// The other file is stored first, so the crash happens in a
			// database that already holds evidence.
			other := l.files[1-c.file]
			l.must("store", other)
			l.crash(spec(c.point, "exit:3"+c.at), "store", l.files[c.file])

			l.must("repair", "--fix")
			name := filepath.Base(l.files[c.file])
			if st := l.list().states[name]; !slices.Contains(c.want, st) {
				t.Fatalf("after recovery %s is %q, want one of %v", name, st, c.want)
			}
			l.recoverAndCheck(want)
		})
	}
}

// TestKilledDuringIndexing kills a store from outside while it is blocked in
// the index pass: no exit path of dues runs at all.
func TestKilledDuringIndexing(t *testing.T) {
	ref := reference(t)
	l := newCrashLab(t)
	l.must("store", l.files[0])
	args := []string{"store", l.files[1], "--dbpath", l.root, "-c", "64"}
	c := testutil.StartDues(t, l.bin, l.work, []string{"DUES_FAULTS=" + spec(faultinject.IndexMid, "block@3")}, args...)
	c.WaitForPoint(t, faultinject.IndexMid+"#blocked", time.Minute)
	c.Kill(t)
	l.recoverAndCheck(ref)
}

// TestCrashWritingTheManifest kills the store that creates a database after
// its manifest's temporary file is written.
func TestCrashWritingTheManifest(t *testing.T) {
	ref := reference(t)
	l := newCrashLab(t)
	l.crash(spec(faultinject.ManifestTempWritten, "exit:3"), "store", l.files[0])
	l.recoverAndCheck(ref)
}

// TestCrashDuringRestore kills a restore part way: the target is untouched,
// and the next restore to it leaves nothing behind.
func TestCrashDuringRestore(t *testing.T) {
	l := newCrashLab(t)
	l.storeAll()
	h := l.list().hashes
	out := filepath.Join(l.work, "out", "restored.bin")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, force := range []bool{false, true} {
		args := []string{"restore", h[0], "-f", out, "--force"}
		var before string
		if force {
			if err := os.WriteFile(out, []byte("the examiner's file"), 0o644); err != nil {
				t.Fatal(err)
			}
			before = testutil.FileSHA256(t, out)
		}
		l.crash(spec(faultinject.RestoreMid, "exit:3@2"), args...)
		switch _, err := os.Stat(out); {
		case !force && err == nil:
			t.Fatal("a killed restore created its target")
		case force && testutil.FileSHA256(t, out) != before:
			t.Fatal("a killed restore changed the file it was replacing")
		}

		l.must(args...)
		if sum := testutil.FileSHA256(t, out); !l.sums[sum] {
			t.Fatalf("restored content matches no input (%s)", sum)
		}
		if left := testutil.FilesMatching(t, filepath.Dir(out), "*.dues-tmp-*"); len(left) > 0 {
			t.Fatalf("left beside the target: %v", left)
		}
		if err := os.Remove(out); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCrashDuringReset kills a reset after each entry it removes. Whatever is
// left must be either no database or a consistent one, and a second reset
// must finish.
func TestCrashDuringReset(t *testing.T) {
	for n := 1; n <= 7; n++ {
		t.Run(fmt.Sprintf("after-%d", n), func(t *testing.T) {
			l := newCrashLab(t)
			// A graph and a full-text index give the reset more to remove.
			for _, f := range l.files {
				l.must("store", f, "--fts", "--enrich")
			}
			r := l.dues(spec(faultinject.ResetMid, fmt.Sprintf("exit:3@%d", n)), "reset", "--yes")
			if r.ExitCode == 0 {
				t.Skipf("reset removed fewer than %d entries", n)
			}
			if r.ExitCode != crashExit {
				t.Fatalf("reset: exit %d\n%s", r.ExitCode, r.Combined())
			}

			switch lr := l.dues("", "list"); {
			case lr.ExitCode == 0:
				// What is left opens, so it must be whole.
				l.checkStored(l.list())
			case !strings.Contains(lr.Combined(), "no DUES database"):
				t.Fatalf("list after an interrupted reset: exit %d\n%s", lr.ExitCode, lr.Combined())
			}
			// What is left takes new evidence.
			l.must("store", l.files[0])
			l.checkStored(l.list())
			l.must("reset", "--yes")
			if _, err := os.Stat(l.root); err == nil {
				entries, _ := os.ReadDir(l.root)
				t.Fatalf("the data root survived a reset: %v", entries)
			}
		})
	}
}

// TestCrashDuringUpload kills the server while it finalizes an upload,
// before and after the upload is stored. A later writable open recovers the
// database and removes the part file, and storing the files again leaves
// what an uninterrupted run leaves.
func TestCrashDuringUpload(t *testing.T) {
	ref := reference(t)
	for _, point := range []string{faultinject.UploadBeforeIngest, faultinject.UploadAfterIngest} {
		t.Run(point, func(t *testing.T) {
			l := newCrashLab(t)
			l.must("store", l.files[1])
			addr := freeAddr(t)
			srv := testutil.StartDues(t, l.bin, l.work, []string{"DUES_FAULTS=" + spec(point, "exit:3")},
				"server", "--listen", addr, "--dbpath", l.root, "-c", "64")
			waitHealthy(t, addr, srv)
			if err := upload(addr, l.files[0]); err == nil {
				t.Fatal("the upload finished, though the server was to die finalizing it")
			}
			if code := srv.Wait(t, time.Minute); code != crashExit {
				t.Fatalf("server: exit %d, want %d\n%s", code, crashExit, srv.Output())
			}
			if parts := testutil.FilesMatching(t, l.root, "*.part"); len(parts) == 0 {
				t.Fatal("the killed server left no part file, so this test checks nothing")
			}
			l.recoverAndCheck(ref)
		})
	}
}

// freeAddr returns a loopback address no one is listening on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func waitHealthy(t *testing.T, addr string, srv *testutil.Child) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server at %s never became healthy\n%s", addr, srv.Output())
}

// upload sends path to the server with upload v2, in one chunk.
func upload(addr, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sum := blake3.Sum256(data)
	size := int64(len(data))
	var started struct {
		UploadID string `json:"upload_id"`
	}
	start := map[string]any{"file_path": filepath.Base(path), "file_hash": base64.StdEncoding.EncodeToString(sum[:]), "size": size}
	if err := postJSON(addr, "/web/upload/start", start, &started); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/web/upload/chunk", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Upload-Id", started.UploadID)
	req.Header.Set("Upload-Offset", "0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("chunk: %s", resp.Status)
	}
	return postJSON(addr, "/web/upload/finalize", map[string]any{"upload_id": started.UploadID}, nil)
}

func postJSON(addr, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := http.Post("http://"+addr+path, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s: %s", path, resp.Status, msg)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// TestCrashWritingAnArchive kills the compression command before an archive
// is closed: no archive appears, an earlier one is unchanged, and the next
// write of it leaves nothing behind.
func TestCrashWritingAnArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the compression binary")
	}
	bin := testutil.BuildBinary(t, "./cmd/compression", "faultinject")
	work := t.TempDir()
	src := filepath.Join(work, "case")
	testutil.WriteRandomFile(t, src, "a.bin", 1, 300<<10)
	testutil.WriteRandomFile(t, filepath.Join(src, "sub"), "b.bin", 2, 5000)
	for _, format := range []string{"duesarc", "zip"} {
		t.Run(format, func(t *testing.T) {
			out := filepath.Join(work, "out", "case."+format)
			args := []string{"create-archive", "--input", src, "--output", out, "--format", format}
			fault := []string{"DUES_FAULTS=" + spec(faultinject.QwikpakArchiveBeforeClose, "exit:3")}
			for _, earlier := range []bool{false, true} {
				var before string
				if earlier {
					before = testutil.FileSHA256(t, out)
				}
				if r := testutil.RunDuesEnv(t, bin, work, 2*time.Minute, fault, args...); r.ExitCode != crashExit {
					t.Fatalf("create-archive: exit %d, want %d\n%s", r.ExitCode, crashExit, r.Combined())
				}
				switch _, err := os.Stat(out); {
				case !earlier && err == nil:
					t.Fatal("a killed archive write created its output")
				case earlier && testutil.FileSHA256(t, out) != before:
					t.Fatal("a killed archive write changed the archive it was replacing")
				}
				if r := testutil.RunDuesEnv(t, bin, work, 2*time.Minute, nil, args...); r.ExitCode != 0 {
					t.Fatalf("create-archive: exit %d\n%s", r.ExitCode, r.Combined())
				}
				if left := testutil.FilesMatching(t, filepath.Dir(out), "*.dues-tmp-*"); len(left) > 0 {
					t.Fatalf("left beside the archive: %v", left)
				}
			}
			dst := filepath.Join(work, "extracted-"+format)
			if r := testutil.RunDuesEnv(t, bin, work, 2*time.Minute, nil, "extract-archive", "--input", out, "--output", dst); r.ExitCode != 0 {
				t.Fatalf("extract-archive: exit %d\n%s", r.ExitCode, r.Combined())
			}
			if got, want := testutil.TreeHash(t, dst), testutil.TreeHash(t, src); got != want {
				t.Fatal("the archive does not extract to its input")
			}
		})
	}
}
