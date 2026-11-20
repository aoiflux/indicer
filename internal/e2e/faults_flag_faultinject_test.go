//go:build faultinject

package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"indicer/internal/faultinject"
	"indicer/internal/testutil"
)

// TestFaultsFlag: a faultinject build injects what --faults says, reports
// each point's first hit to --faults-notify, and refuses a bad spec.
func TestFaultsFlag(t *testing.T) {
	bin := testutil.BuildDues(t, "faultinject")
	work := t.TempDir()
	ev := testutil.WriteRandomFile(t, work, "ev.bin", 3, 64<<10)
	notify := filepath.Join(work, "points.log")

	r := testutil.RunDues(t, bin, work, 2*time.Minute,
		"--faults="+faultinject.IngestBeforeCommit+"=exit:3", "--faults-notify="+notify,
		"store", ev, "--dbpath", filepath.Join(work, "db"))
	if r.ExitCode != 3 {
		t.Fatalf("store: exit %d, want the injected 3\n%s", r.ExitCode, r.Combined())
	}
	b, err := os.ReadFile(notify)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(strings.Split(string(b), "\n"), faultinject.IngestBeforeCommit) {
		t.Errorf("--faults-notify file does not report %s:\n%s", faultinject.IngestBeforeCommit, b)
	}

	r = testutil.RunDues(t, bin, work, time.Minute, "--faults=no.such.point=error", "version")
	if r.ExitCode != 2 || !strings.Contains(r.Stderr, "--faults") {
		t.Errorf("bad spec: exit %d, want 2 naming the flag\n%s", r.ExitCode, r.Combined())
	}
}
