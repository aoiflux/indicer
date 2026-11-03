package e2e

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"indicer/internal/testutil"
	"indicer/internal/testutil/vtscreen"
)

// ptyScript returns util-linux script(1), which runs a command on a
// pseudo-terminal, or skips the test where there is none. Built with -tags
// requirepty, as the WSL suite builds it, a skip is a failure.
func ptyScript(t *testing.T) string {
	t.Helper()
	skip := func(why string) {
		t.Helper()
		if requirePTY {
			t.Fatalf("no pseudo-terminal test: %s", why)
		}
		t.Skip(why)
	}
	if testing.Short() {
		skip("short mode")
	}
	if runtime.GOOS != "linux" {
		skip("needs util-linux script(1)")
	}
	path, err := exec.LookPath("script")
	if err != nil {
		skip("no script(1)")
	}
	// BusyBox and BSD script(1) take other arguments.
	if err := exec.Command(path, "-qec", "true", "/dev/null").Run(); err != nil {
		skip("script(1) is not util-linux's: " + err.Error())
	}
	return path
}

// ptyEnv is script(1)'s whole environment: what its shell and stty need.
// SHELL is set because the user's login shell may not be a POSIX one
// (fish). DUES reads none of it; its settings are arguments.
var ptyEnv = []string{"PATH=/usr/bin:/bin", "SHELL=/bin/sh"}

// runPTY runs dues with args on a pseudo-terminal cols columns wide, in
// work, and returns what the terminal received and the exit code. stty sets
// the size, so DUES reads it the way it reads a real terminal's. No extra
// plays, no config file, and the log goes to a test directory.
func runPTY(t *testing.T, script, bin, work string, cols int, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	line := "stty cols " + strconv.Itoa(cols) + " rows 30; exec " + shQuote(bin)
	for _, a := range testutil.IsolatedArgs(t, append([]string{"--no-fun"}, args...)) {
		line += " " + shQuote(a)
	}
	cmd := exec.CommandContext(ctx, script, "-qec", line, "/dev/null")
	cmd.Dir, cmd.Env = work, ptyEnv
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	// Open until the command ends, so script(1) never sees its input end.
	if _, err := cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("script: %v\n%q", err, out.String())
		}
		code = exit.ExitCode()
	}
	return out.String(), code
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var ptyHeader = regexp.MustCompile(`^◆ DUES v\S+ · store · ev\.bin →`)

// TestLiveDisplayInPTY runs DUES on a real pseudo-terminal and holds the
// live display to what a person sees: a store redrawn in place with only
// the escape sequences DUES may write, the cursor shown at the end, and the
// report and footer left behind. The screen wraps as a terminal does: the
// report lines on stdout may be longer than the terminal is wide.
func TestLiveDisplayInPTY(t *testing.T) {
	script := ptyScript(t)
	bin := testutil.BuildDues(t)
	for _, cols := range []int{100, 50} {
		t.Run(strconv.Itoa(cols)+" columns", func(t *testing.T) {
			work := t.TempDir()
			testutil.WriteRandomFile(t, work, "ev.bin", 7, 16<<20)
			out, code := runPTY(t, script, bin, work, cols, "store", "ev.bin", "--dbpath", "db", "--compress-level", "fast")
			if code != 0 {
				t.Fatalf("exit %d:\n%q", code, out)
			}
			scr := vtscreen.New(t, cols, vtscreen.AllowWrap())
			scr.Write([]byte(out))
			scr.Finish()
			hide, show := strings.LastIndex(out, "\x1b[?25l"), strings.LastIndex(out, "\x1b[?25h")
			if hide < 0 || show < hide || scr.Hidden() {
				t.Errorf("cursor: last hidden at %d, last shown at %d, hidden at the end %v", hide, show, scr.Hidden())
			}
			if lines := scr.Text(); len(lines) == 0 || !ptyHeader.MatchString(lines[0]) {
				t.Fatalf("no run header:\n%s", scr)
			}
			checkFinalScreen(t, scr)
		})
	}

	// On a terminal, version is the banner and an overview; --version is
	// the identity line alone.
	work := t.TempDir()
	out, code := runPTY(t, script, bin, work, 100, "version")
	scr := vtscreen.New(t, 100)
	scr.Write([]byte(out))
	scr.Finish()
	if s := scr.String(); code != 0 || !strings.Contains(s, "●─●─●  ┌─╮ ╷ ╷ ┌─╴ ╭─╴  Deduplicated Unified Evidence Store") || !strings.Contains(s, "Overview") {
		t.Errorf("dues version, exit %d:\n%s", code, s)
	}
	out, code = runPTY(t, script, bin, work, 100, "--version")
	scr = vtscreen.New(t, 100)
	scr.Write([]byte(out))
	scr.Finish()
	if lines := scr.Text(); code != 0 || len(lines) != 1 || !strings.HasPrefix(lines[0], "DUES ") || !strings.HasSuffix(lines[0], " - Deduplicated Unified Evidence Store") {
		t.Errorf("dues --version, exit %d:\n%s", code, scr)
	}
}

// checkFinalScreen holds a finished store's screen to what it must show:
// the report once, each step's row once, the footer last, and nothing left
// of the live display (no spinner, no bar) or of plain output. A redraw
// that went wrong, a wrapped row say, leaves rows behind.
func checkFinalScreen(t *testing.T, scr *vtscreen.Screen) {
	t.Helper()
	lines := scr.Text()
	rows := map[string]int{}
	stored := 0
	last := "" // the last line that is not a continuation (DUES indents those)
	for _, l := range lines {
		if !strings.HasPrefix(l, " ") {
			last = l
		}
		switch {
		case strings.HasPrefix(l, "Stored as evidence "):
			stored++
		case strings.HasPrefix(l, "dues: store:"):
			t.Errorf("a plain progress line on a terminal: %q", l)
		case strings.ContainsAny(l, "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏━╸"):
			t.Errorf("the live display left a row behind: %q", l)
		case strings.HasPrefix(l, "  ✓ "):
			rows[strings.Fields(l)[1]]++
		}
	}
	for _, step := range []string{"store", "index", "compact"} {
		if rows[step] != 1 {
			t.Errorf("the %s row shows %d times:\n%s", step, rows[step], scr)
		}
	}
	if stored != 1 || !strings.HasPrefix(last, "✓ done in ") {
		t.Errorf("screen:\n%s", scr)
	}
}
