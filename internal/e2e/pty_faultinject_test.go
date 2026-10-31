//go:build faultinject

package e2e

import (
	"regexp"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/vtscreen"
)

var redraw = regexp.MustCompile(`\r\x1b\[\d+A`)

// TestTaskbarInPTY: a store that pauses before its commit keeps the live
// display up long enough to redraw, and under Windows Terminal to show its
// progress on the tab, which is cleared at the end. Elsewhere it writes no
// OSC at all (the screen fails the test on one).
func TestTaskbarInPTY(t *testing.T) {
	script := ptyScript(t)
	bin := testutil.BuildDues(t, "faultinject")
	for name, wt := range map[string]bool{"windows terminal": true, "other terminal": false} {
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			testutil.WriteRandomFile(t, work, "ev.bin", 7, 16<<20)
			extra := []string{"DUES_FAULTS=ingest.before_commit=sleep:2s"}
			opts := []vtscreen.Option{vtscreen.AllowWrap()}
			if wt {
				extra = append(extra, "WT_SESSION=e2e")
				opts = append(opts, vtscreen.AllowTaskbar())
			}
			out, code := runPTY(t, script, bin, work, 100, ptyEnv(t, extra...), "store", "ev.bin", "--dbpath", "db", "--compress-level", "fast")
			if code != 0 {
				t.Fatalf("exit %d:\n%q", code, out)
			}
			scr := vtscreen.New(t, 100, opts...)
			scr.Write([]byte(out))
			scr.Finish()
			if n := len(redraw.FindAllString(out, -1)); n < 2 {
				t.Errorf("%d redraws in a run of over two seconds", n)
			}
			checkFinalScreen(t, scr)
			if states := scr.Taskbar(); wt && (len(states) == 0 || states[len(states)-1] != "0;0") {
				t.Errorf("taskbar states %q", states)
			}
		})
	}
}
