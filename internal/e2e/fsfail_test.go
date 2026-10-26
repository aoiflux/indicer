package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"indicer/internal/testutil"
)

// The filesystem-failure tests run where scripts/wsl/mounts.sh made the
// special filesystems (the verify gate's WSL suite) and skip elsewhere.
// Their names match the suite's filter: DiskFull, ReadOnly, Permission.

// duesRun runs the dues binary and returns its result.
func duesRun(t *testing.T, bin, work string, args ...string) testutil.Result {
	t.Helper()
	return testutil.RunDues(t, bin, work, 5*time.Minute, args...)
}

func duesMust(t *testing.T, bin, work string, args ...string) testutil.Result {
	t.Helper()
	r := duesRun(t, bin, work, args...)
	if r.ExitCode != 0 {
		t.Fatalf("dues %s: exit %d\n%s", strings.Join(args, " "), r.ExitCode, r.Combined())
	}
	return r
}

// failsCleanly checks a command failed with an error, not a crash: exit 1 or
// 2 with "dues: error:", and none of the Go runtime's crash output.
func failsCleanly(t *testing.T, r testutil.Result, want string, args ...string) {
	t.Helper()
	out := r.Combined()
	if r.ExitCode == 0 || strings.Contains(out, "fatal error:") || strings.Contains(out, "SIGBUS") || strings.Contains(out, "panic:") {
		t.Fatalf("dues %s: exit %d, want a clean failure\n%s", strings.Join(args, " "), r.ExitCode, out)
	}
	if !strings.Contains(out, want) {
		t.Fatalf("dues %s: exit %d without %q\n%s", strings.Join(args, " "), r.ExitCode, want, out)
	}
}

// tempIn makes a directory in dir that the test removes at its end.
func tempIn(t *testing.T, dir string) string {
	t.Helper()
	d, err := os.MkdirTemp(dir, "dues-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// fill writes zeros into dir until at most keep bytes are left.
func fill(t *testing.T, dir string, keep int64) {
	t.Helper()
	f, err := os.CreateTemp(dir, "fill-")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	block := make([]byte, 64<<10)
	for {
		if _, err := f.Write(block); err != nil {
			break
		}
	}
	// Leave keep bytes: give back what the last writes took.
	if fi, err := f.Stat(); err == nil && keep > 0 {
		_ = f.Truncate(max(fi.Size()-keep, 0))
	}
}

type listedEvidence struct {
	Hash   string `json:"hash"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

func listEvidence(t *testing.T, bin, work, root string) []listedEvidence {
	t.Helper()
	r := duesMust(t, bin, work, "list", "--dbpath", root)
	i, j := strings.Index(r.Stdout, "{"), strings.LastIndex(r.Stdout, "}")
	var doc struct {
		Evidence []listedEvidence `json:"evidence"`
	}
	if i < 0 || j < i || json.Unmarshal([]byte(r.Stdout[i:j+1]), &doc) != nil {
		t.Fatalf("list output:\n%s", r.Combined())
	}
	return doc.Evidence
}

// TestStoreDiskFullRefusedUpFront: on a volume with less free than DUES's
// reserve, a store fails before writing anything. badger used to crash the
// process with SIGBUS when the disk filled under one of its mapped files.
func TestStoreDiskFullRefusedUpFront(t *testing.T) {
	small := testutil.RequireEnvDir(t, "DUES_TEST_ENOSPC_DIR")
	bin := testutil.BuildDues(t)
	work := t.TempDir()
	src := testutil.WriteRandomFile(t, work, "a.bin", 1, 200<<10)
	root := filepath.Join(tempIn(t, small), "db")

	args := []string{"store", src, "--dbpath", root}
	failsCleanly(t, duesRun(t, bin, work, args...), "not enough free disk space", args...)
	if r := duesRun(t, bin, work, "list", "--dbpath", root); !strings.Contains(r.Combined(), "no DUES database") {
		t.Fatalf("list after a refused store: exit %d\n%s", r.ExitCode, r.Combined())
	}
}

// TestStoreDiskFullMidway fills the volume during a store: the store fails
// with a clear error, the database stays whole, and earlier evidence still
// restores verified.
func TestStoreDiskFullMidway(t *testing.T) {
	low := testutil.RequireEnvDir(t, "DUES_TEST_LOWSPACE_DIR")
	bin := testutil.BuildDues(t)
	work := t.TempDir()
	root := filepath.Join(tempIn(t, low), "db")
	first := testutil.WriteRandomFile(t, work, "first.bin", 1, 64<<20)
	big := testutil.WriteRandomFile(t, work, "big.bin", 2, 320<<20)

	duesMust(t, bin, work, "store", first, "--dbpath", root)
	args := []string{"store", big, "--dbpath", root}
	failsCleanly(t, duesRun(t, bin, work, args...), "not enough free disk space", args...)

	var firstHash string
	for _, e := range listEvidence(t, bin, work, root) {
		switch e.Name {
		case "first.bin":
			firstHash = e.Hash
			if e.Status != "completed" {
				t.Fatalf("first.bin is %s", e.Status)
			}
		case "big.bin":
			if e.Status == "completed" {
				t.Fatal("the store that ran out of space completed")
			}
		}
	}
	out := filepath.Join(work, "restored.bin")
	duesMust(t, bin, work, "restore", firstHash, "-f", out, "--verify", "full", "--dbpath", root)
	if testutil.FileSHA256(t, out) != testutil.FileSHA256(t, first) {
		t.Fatal("first.bin restored to other content")
	}
	duesMust(t, bin, work, "repair", "--verify", "--dbpath", root)
	// Still no room: the resumed store fails the same way.
	failsCleanly(t, duesRun(t, bin, work, args...), "not enough free disk space", args...)
}

// TestOutputsDiskFull writes a restore, a search report and an archive to a
// full volume: each fails, leaves no output or temporary file there, and the
// database (elsewhere) is untouched.
func TestOutputsDiskFull(t *testing.T) {
	small := testutil.RequireEnvDir(t, "DUES_TEST_ENOSPC_DIR")
	bin := testutil.BuildDues(t)
	work := t.TempDir()
	root := filepath.Join(work, "db")
	src := testutil.WriteRandomFile(t, work, "a.bin", 1, 24<<20)
	duesMust(t, bin, work, "store", src, "--dbpath", root)
	hash := listEvidence(t, bin, work, root)[0].Hash
	outDir := tempIn(t, small)
	fill(t, outDir, 1<<20)

	target := filepath.Join(outDir, "restored.bin")
	args := []string{"restore", hash, "-f", target, "--dbpath", root}
	failsCleanly(t, duesRun(t, bin, work, args...), "no space left on device", args...)

	report := filepath.Join(outDir, "report.json")
	fill(t, outDir, 0)
	args = []string{"search", "abc", "--out", report, "--dbpath", root}
	failsCleanly(t, duesRun(t, bin, work, args...), "no space left on device", args...)

	comp := testutil.BuildBinary(t, "./cmd/compression")
	archive := filepath.Join(outDir, "a.zip")
	if r := duesRun(t, comp, work, "create-archive", "--input", src, "--output", archive, "--format", "zip"); r.ExitCode == 0 || strings.Contains(r.Combined(), "fatal error:") {
		t.Fatalf("create-archive on a full volume: exit %d\n%s", r.ExitCode, r.Combined())
	}

	for _, p := range []string{target, report, archive} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s was written to a full volume", filepath.Base(p))
		}
	}
	if left := testutil.FilesMatching(t, outDir, "*.dues-tmp-*"); len(left) > 0 {
		t.Fatalf("left on the full volume: %v", left)
	}
	duesMust(t, bin, work, "repair", "--verify", "--dbpath", root)
}

// TestLogDirDiskFull: a full log volume does not stop a command.
func TestLogDirDiskFull(t *testing.T) {
	small := testutil.RequireEnvDir(t, "DUES_TEST_ENOSPC_DIR")
	bin := testutil.BuildDues(t)
	work := t.TempDir()
	logs := tempIn(t, small)
	fill(t, logs, 0)
	src := testutil.WriteRandomFile(t, work, "a.bin", 1, 1<<20)
	duesMust(t, bin, work, "--log-dir", logs, "store", src, "--dbpath", filepath.Join(work, "db"))
}

// TestReadOnlyMediaDatabase reads a database through a read-only mount, as
// from write-protected media: every read command works, every write command
// fails with an error, and nothing changes.
func TestReadOnlyMediaDatabase(t *testing.T) {
	srcDir := testutil.RequireEnvDir(t, "DUES_TEST_RO_SRC")
	roDir := testutil.RequireEnvDir(t, "DUES_TEST_RO_DB")
	bin := testutil.BuildDues(t)
	work := t.TempDir()
	case_ := tempIn(t, srcDir)
	root := filepath.Join(case_, "db")
	files := []string{
		testutil.WriteRandomFile(t, work, "a.bin", 1, 2<<20),
		copyFile(t, fsfixtureImage(t, "fat/fat16"), filepath.Join(work, "fat16.img")),
	}
	for _, f := range files {
		duesMust(t, bin, work, "store", f, "--revrel", "--enrich", "--fts", "--dbpath", root)
	}
	duesMust(t, bin, work, "micro", "extract", "--out", filepath.Join(work, "graph.html"), "--dbpath", root)
	before := testutil.TreeHash(t, root)
	ro := filepath.Join(roDir, filepath.Base(case_), "db")
	hash := listEvidence(t, bin, work, ro)[0].Hash

	for _, args := range [][]string{
		{"list"},
		{"stats"},
		{"search", "abc", "--out", filepath.Join(work, "report.json")},
		{"search", "abc", "--fts", "--out", filepath.Join(work, "report-fts.json")},
		{"restore", hash, "-f", filepath.Join(work, "restored.bin")},
		{"near", "in", hash, "--out", filepath.Join(work, "near.json")},
		{"micro", "list"},
		{"repair"},
		{"repair", "--verify"},
	} {
		duesMust(t, bin, work, append(args, "--dbpath", ro)...)
	}
	for _, args := range [][]string{
		{"store", files[0]},
		{"repair", "--fix"},
		{"reindex", "--all"},
		{"enrich"},
		{"micro", "extract", "--out", filepath.Join(work, "g2.html")},
		{"reset", "--yes"},
	} {
		args = append(args, "--dbpath", ro)
		failsCleanly(t, duesRun(t, bin, work, args...), "read-only", args...)
	}
	if testutil.TreeHash(t, root) != before {
		t.Fatal("the database changed through a read-only mount")
	}
}

// TestPermissionUnreadableBlob: a chunk blob the user cannot read fails a
// restore and a verification with an error naming it; once readable again,
// the evidence restores.
func TestPermissionUnreadableBlob(t *testing.T) {
	testutil.RequireLinuxNonRoot(t)
	bin := testutil.BuildDues(t)
	work := t.TempDir()
	root := filepath.Join(work, "db")
	src := testutil.WriteRandomFile(t, work, "a.bin", 1, 1<<20)
	duesMust(t, bin, work, "store", src, "--dbpath", root)
	hash := listEvidence(t, bin, work, root)[0].Hash
	blobs, err := filepath.Glob(filepath.Join(root, "blob", "*.blob"))
	if err != nil || len(blobs) == 0 {
		t.Fatalf("no blobs: %v", err)
	}
	if err := os.Chmod(blobs[0], 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blobs[0], 0o644) })

	out := filepath.Join(work, "restored.bin")
	args := []string{"restore", hash, "-f", out, "--dbpath", root}
	failsCleanly(t, duesRun(t, bin, work, args...), "permission denied", args...)
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a failed restore wrote its target")
	}
	args = []string{"repair", "--verify", "--dbpath", root}
	failsCleanly(t, duesRun(t, bin, work, args...), filepath.Base(blobs[0]), args...)

	if err := os.Chmod(blobs[0], 0o644); err != nil {
		t.Fatal(err)
	}
	duesMust(t, bin, work, "restore", hash, "-f", out, "--dbpath", root)
}

// TestPermissionUnreadableSource: a source file the user cannot read is not
// stored, and nothing is recorded for it.
func TestPermissionUnreadableSource(t *testing.T) {
	testutil.RequireLinuxNonRoot(t)
	bin := testutil.BuildDues(t)
	work := t.TempDir()
	root := filepath.Join(work, "db")
	duesMust(t, bin, work, "store", testutil.WriteRandomFile(t, work, "ok.bin", 1, 1<<20), "--dbpath", root)
	src := testutil.WriteRandomFile(t, work, "secret.bin", 2, 1<<20)
	if err := os.Chmod(src, 0); err != nil {
		t.Fatal(err)
	}
	args := []string{"store", src, "--dbpath", root}
	failsCleanly(t, duesRun(t, bin, work, args...), "permission denied", args...)
	if got := listEvidence(t, bin, work, root); len(got) != 1 {
		t.Fatalf("%d evidence listed after an unreadable source, want 1", len(got))
	}
}

// TestPermissionReadOnlyDataRoot: a data root the user may only read works
// for read commands and refuses write commands with an error.
func TestPermissionReadOnlyDataRoot(t *testing.T) {
	testutil.RequireLinuxNonRoot(t)
	bin := testutil.BuildDues(t)
	work := t.TempDir()
	root := filepath.Join(work, "db")
	src := testutil.WriteRandomFile(t, work, "a.bin", 1, 1<<20)
	duesMust(t, bin, work, "store", src, "--dbpath", root)
	hash := listEvidence(t, bin, work, root)[0].Hash
	chmodTree(t, root, 0o555, 0o444)
	t.Cleanup(func() { chmodTree(t, root, 0o755, 0o644) })

	duesMust(t, bin, work, "restore", hash, "-f", filepath.Join(work, "restored.bin"), "--dbpath", root)
	duesMust(t, bin, work, "repair", "--verify", "--dbpath", root)
	args := []string{"store", testutil.WriteRandomFile(t, work, "b.bin", 2, 1<<20), "--dbpath", root}
	failsCleanly(t, duesRun(t, bin, work, args...), "permission denied", args...)
}

// TestPermissionLogDir: a log directory the user cannot write does not stop
// a command.
func TestPermissionLogDir(t *testing.T) {
	testutil.RequireLinuxNonRoot(t)
	bin := testutil.BuildDues(t)
	work := t.TempDir()
	logs := filepath.Join(work, "logs")
	if err := os.Mkdir(logs, 0o555); err != nil {
		t.Fatal(err)
	}
	src := testutil.WriteRandomFile(t, work, "a.bin", 1, 1<<20)
	duesMust(t, bin, work, "--log-dir", logs, "store", src, "--dbpath", filepath.Join(work, "db"))
}

func chmodTree(t *testing.T, root string, dirMode, fileMode os.FileMode) {
	t.Helper()
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		return os.Chmod(p, fileMode)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Directories last, deepest first, so a walk never loses access.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i], dirMode); err != nil {
			t.Fatal(err)
		}
	}
}
