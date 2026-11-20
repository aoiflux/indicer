//go:build faultinject

package e2e

import (
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/faultinject"
	"indicer/internal/testutil"
)

// TestPackageMergeFaults: an evidence whose index fails part way merges
// with a warning, and a rerun finds it present and still says so; a merge
// killed before its commit keeps its copy of the package, and the rerun
// recovers the evidence, says its index needs the examiner, and leaves no
// copy; once the index is run again, a merge finds it present.
func TestPackageMergeFaults(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the dues binary")
	}
	bin := testutil.BuildDues(t, "faultinject")
	work := t.TempDir()
	copyFile(t, fsfixtureImage(t, "fat/fat16"), filepath.Join(work, "fat16.img"))
	db := filepath.Join(work, "case")
	duesMust(t, bin, work, "store", "fat16.img", "--dbpath", db)
	duesMust(t, bin, work, "package", "export", "--dbpath", db, "--out", "case.duesarc")
	merge := func(faults, target string) testutil.Result {
		t.Helper()
		return duesRun(t, bin, work, append(testutil.FaultArgs(faults), "package", "merge", "case.duesarc", "--dbpath", target)...)
	}

	partial := filepath.Join(work, "partial")
	for _, c := range []struct{ faults, outcome string }{
		{spec(faultinject.IndexMid, "error"), "merged"},
		{"", "present"},
	} {
		r := merge(c.faults, partial)
		if r.ExitCode != 2 || !hasOutcome(r.Stdout, c.outcome, "fat16.img") || !strings.Contains(r.Stdout, "  warning: ") {
			t.Fatalf("merge with faults %q: exit %d, want 2 with fat16.img %s and a warning\n%s", c.faults, r.ExitCode, c.outcome, r.Combined())
		}
	}
	noStageLeft(t, work)

	killed := filepath.Join(work, "killed")
	if r := merge(spec(faultinject.IngestBeforeCommit, "exit:3"), killed); r.ExitCode != crashExit {
		t.Fatalf("merge killed before its commit: exit %d, want %d\n%s", r.ExitCode, crashExit, r.Combined())
	}
	if len(testutil.FilesMatching(t, work, "*.dues-merge-*")) == 0 {
		t.Fatal("a killed merge kept no copy of the package")
	}
	// Recovery commits the evidence, but cannot know its index pass ended,
	// as after a store killed there.
	r := merge("", killed)
	if r.ExitCode != 2 || !hasOutcome(r.Stdout, "present", "fat16.img") || !strings.Contains(r.Stdout, "its index is interrupted") {
		t.Fatalf("rerun of a killed merge: exit %d, want 2 with fat16.img present and its index interrupted\n%s", r.ExitCode, r.Combined())
	}
	noStageLeft(t, work)
	duesMust(t, bin, work, "repair", "--fix", "--reindex-failed", "--dbpath", killed)
	if r := merge("", killed); r.ExitCode != 0 || !hasOutcome(r.Stdout, "present", "fat16.img") {
		t.Fatalf("merge after the reindex: exit %d\n%s", r.ExitCode, r.Combined())
	}
	if m := mergedEvidence(t, bin, work, killed).Merged; m == nil || m.SourceEvidenceID == "" {
		t.Errorf("the recovered evidence does not say where it was merged from: %+v", m)
	}
}
