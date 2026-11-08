package e2e

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"indicer/internal/testutil"
)

// TestEnvironmentIsIgnored: dues reads no environment variable. The same
// commands run twice, once with an empty environment and once with every
// variable DUES, or a convention it once followed, read set to a value that
// would have changed what it did. Both print the same, and nothing is
// written where the variables point.
func TestEnvironmentIsIgnored(t *testing.T) {
	t.Parallel()
	bin := testutil.BuildDues(t)
	junk := t.TempDir()
	dir := func(name string) string {
		t.Helper()
		d := filepath.Join(junk, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	}
	tmp := dir("tmp")
	set := []string{
		// DUES's own variables, all retired.
		"DUES_COLOR=always", "DUES_PLAIN=1", "DUES_NO_FUN=1", "DUES_THEME=light", "DUES_DEBUG=1",
		"DUES_LOG_DIR=" + dir("logs"), "DUES_PASSWORD=junk-password", "DUES_TOKEN=junk-token",
		"DUES_SAFE_STRICT=1", "DUES_FAULTS=ingest.before_commit=exit:9",
		// Colour and CI conventions.
		"NO_COLOR=1", "FORCE_COLOR=1", "CLICOLOR_FORCE=1", "CI=true", "GITHUB_ACTIONS=true",
		// Terminal facts.
		"TERM=dumb", "COLORTERM=truecolor", "TERM_PROGRAM=vscode", "WT_SESSION=e2e", "ConEmuANSI=ON",
		"TMUX=/tmp/tmux-0/default,1,0", "STY=1.pts-0", "COLUMNS=10", "LINES=5", "COLORFGBG=0;15",
		"LANG=ja_JP.UTF-8", "LC_ALL=ja_JP.UTF-8", "RUNEWIDTH_EASTASIAN=1",
		// The directories and the account.
		"HOME=" + dir("home"), "USERPROFILE=" + dir("profile"), "LOCALAPPDATA=" + dir("localappdata"),
		"APPDATA=" + dir("appdata"), "XDG_CACHE_HOME=" + dir("cache"),
		"TMPDIR=" + tmp, "TMP=" + tmp, "TEMP=" + tmp, "USER=junk", "USERNAME=junk",
	}

	clean := runCommands(t, bin, []string{})
	for _, c := range clean {
		if c.code != 0 {
			t.Fatalf("dues %s with an empty environment: exit %d\n%s%s", c.cmd, c.code, c.stdout, c.stderr)
		}
	}
	if !strings.Contains(clean[2].stdout, "Stored as evidence <idA>") || !strings.Contains(clean[3].stdout, `"id": "<idA>"`) {
		t.Fatalf("store and list did not run as expected:\n%s\n%s", clean[2].stdout, clean[3].stdout)
	}
	junked := runCommands(t, bin, set)
	for i, c := range clean {
		j := junked[i]
		if c.code != j.code || c.stdout != j.stdout || c.stderr != j.stderr {
			t.Errorf("dues %s differs with the variables set:\nexit %d, then %d\n--- stdout ---\n%s\n--- then ---\n%s\n--- stderr ---\n%s\n--- then ---\n%s",
				c.cmd, c.code, j.code, c.stdout, j.stdout, c.stderr, j.stderr)
		}
	}

	err := filepath.WalkDir(junk, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			t.Errorf("dues wrote %s, where a variable pointed", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

type commandRun struct {
	cmd            string
	code           int
	stdout, stderr string
}

// runCommands runs the same commands in a fresh work directory with env as
// the whole environment, and returns their output with what differs from
// one run to the next masked: the work directory, durations and IDs. Plain
// start lines are dropped: one shows only when a sample finds its phase
// still running, so a quick phase has one in some runs and not in others.
func runCommands(t *testing.T, bin string, env []string) []commandRun {
	t.Helper()
	work := t.TempDir()
	testutil.WriteRandomFile(t, work, "ev.bin", 7, 300<<10)
	var runs []commandRun
	for _, args := range [][]string{
		{"--version"},
		{"--help"},
		{"store", "ev.bin", "--dbpath", "db"},
		{"list", "--dbpath", "db"},
	} {
		cmd := exec.Command(bin, testutil.IsolatedArgs(t, args)...)
		cmd.Dir = work
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		code := 0
		done := make(chan error, 1)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { done <- cmd.Wait() }()
		var err error
		select {
		case err = <-done:
		case <-time.After(2 * time.Minute):
			_ = cmd.Process.Kill()
			<-done
			t.Fatalf("dues %s: still running after 2m", strings.Join(args, " "))
		}
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		mask := newMasker(work)
		runs = append(runs, commandRun{
			cmd:    strings.Join(args, " "),
			code:   code,
			stdout: mask.apply(stdout.String()),
			stderr: mask.apply(startLineRE.ReplaceAllString(stderr.String(), "")),
		})
	}
	return runs
}

var (
	durationRE = regexp.MustCompile(`(\d+(\.\d+)?(ns|µs|us|ms|s|m|h))+\b`)
	// An evidence or partition ID: 16 bytes in base64.
	idRE = regexp.MustCompile(`[A-Za-z0-9+/]{22}==`)
	// A plain line saying a phase started: "dues: index: start".
	startLineRE = regexp.MustCompile(`(?m)^dues: [^\n]*: start(, [^\n]*)?\r?\n`)
)

// masker numbers the IDs of one run in the order they appear, so two runs
// that store the same file read the same.
type masker struct {
	work string
	ids  map[string]string
}

func newMasker(work string) *masker { return &masker{work: work, ids: map[string]string{}} }

func (m *masker) apply(s string) string {
	s = strings.ReplaceAll(s, m.work, "<work>")
	// IDs first: an ID can hold what reads as a duration ("T1M3s+Q==").
	s = idRE.ReplaceAllStringFunc(s, func(id string) string {
		if _, ok := m.ids[id]; !ok {
			m.ids[id] = "<id" + string(rune('A'+len(m.ids))) + ">"
		}
		return m.ids[id]
	})
	return durationRE.ReplaceAllString(s, "<duration>")
}
