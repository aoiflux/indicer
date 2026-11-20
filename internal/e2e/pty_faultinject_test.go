//go:build faultinject

package e2e

import (
	"regexp"
	"testing"

	"indicer/internal/faultinject"
	"indicer/internal/testutil"
	"indicer/internal/testutil/vtscreen"
)

var redraw = regexp.MustCompile(`\r\x1b\[\d+A`)

// TestTaskbarInPTY: a store that pauses before its commit keeps the live
// display up long enough to redraw, and with --taskbar to show its progress
// on the tab, which is cleared at the end. Without it there is no OSC at all
// (the screen fails the test on one).
func TestTaskbarInPTY(t *testing.T) {
	script := ptyScript(t)
	bin := testutil.BuildDues(t, "faultinject")
	for name, wt := range map[string]bool{"taskbar": true, "no taskbar": false} {
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			testutil.WriteRandomFile(t, work, "ev.bin", 7, 16<<20)
			opts := []vtscreen.Option{vtscreen.AllowWrap()}
			args := []string{"--faults=" + faultinject.IngestBeforeCommit + "=sleep:2s",
				"store", "ev.bin", "--dbpath", "db", "--compress-level", "fast"}
			if wt {
				args = append([]string{"--taskbar"}, args...)
				opts = append(opts, vtscreen.AllowTaskbar())
			}
			out, code := runPTY(t, script, bin, work, 100, args...)
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
