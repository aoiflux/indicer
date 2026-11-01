package e2e

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"indicer/internal/faultinject"
	"indicer/internal/testutil"
)

// TestReleaseBuildRefusesFaults: only a faultinject build takes the fault
// flags, so a release binary cannot be made to inject a fault.
func TestReleaseBuildRefusesFaults(t *testing.T) {
	bin := testutil.BuildDues(t)
	work := t.TempDir()
	for _, args := range [][]string{
		{"--faults=" + faultinject.ResetMid + "=error", "version"},
		{"--faults-notify=" + filepath.Join(work, "points.log"), "version"},
	} {
		r := testutil.RunDues(t, bin, work, time.Minute, args...)
		if r.ExitCode != 2 || !strings.Contains(r.Stderr, "--faults") {
			t.Errorf("%v: exit %d, want 2 naming the flag\n%s", args, r.ExitCode, r.Combined())
		}
	}
}
