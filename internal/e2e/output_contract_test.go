package e2e

import (
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"indicer/internal/testutil"
)

// TestOutputContract runs DUES with pipes, as scripts do, and holds it to
// its output contract (docs/CLI_UX.md): results on stdout, everything else
// on stderr; nothing decorated on a pipe; colour only when asked for.
// testutil.RunDues also fails any run that writes an escape code to a pipe
// without asking for colour.
func TestOutputContract(t *testing.T) {
	t.Parallel()
	bin := testutil.BuildDues(t)
	work := t.TempDir()
	db := filepath.Join(work, "db")
	ev := filepath.Join(work, "ev.bin")
	data := make([]byte, 1<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ev, data, 0o600); err != nil {
		t.Fatal(err)
	}

	r := duesMust(t, bin, work, "store", ev, "--dbpath", db)
	if !strings.Contains(r.Stderr, "dues: store: DUES ") || !strings.Contains(r.Stderr, "dues: store: settings: ") {
		t.Errorf("store: no run header on stderr:\n%s", r.Stderr)
	}
	if strings.Contains(r.Stdout, "dues: store:") || !strings.Contains(r.Stdout, "Stored as evidence ") {
		t.Errorf("store stdout is not the report alone:\n%s", r.Stdout)
	}
	// Asked for taskbar progress, a pipe still gets none: RunDues fails
	// any escape code on it.
	if r := testutil.RunDues(t, bin, work, time.Minute, "--taskbar", "store", ev, "--dbpath", db); r.ExitCode != 0 {
		t.Errorf("store --taskbar: exit %d\n%s", r.ExitCode, r.Combined())
	}

	for _, args := range [][]string{{"list"}, {"micro", "list"}} {
		r := duesMust(t, bin, work, append(args, "--dbpath", db)...)
		var doc any
		if err := json.Unmarshal([]byte(r.Stdout), &doc); err != nil {
			t.Errorf("dues %s: stdout is not one JSON document (%v):\n%s", strings.Join(args, " "), err, r.Stdout)
		}
		if strings.ContainsAny(r.Stderr, "{}") {
			t.Errorf("dues %s: braces on stderr:\n%s", strings.Join(args, " "), r.Stderr)
		}
	}
	// repair prints one JSON document, with a section for each step it ran;
	// the steps' lines go to stderr.
	for _, c := range []struct {
		args          []string
		section, line string
	}{
		{[]string{"repair", "--verify"}, "verify", " 0 damaged.\n"},
		{[]string{"repair", "--fix", "--revrel"}, "revrel", "Reverse relations: "},
	} {
		r := duesMust(t, bin, work, append(c.args, "--dbpath", db)...)
		var doc map[string]json.RawMessage
		if err := json.Unmarshal([]byte(r.Stdout), &doc); err != nil || doc[c.section] == nil {
			t.Errorf("dues %s: stdout is not one JSON document with %q (%v):\n%s", strings.Join(c.args, " "), c.section, err, r.Stdout)
		}
		if !strings.Contains(r.Stderr, c.line) {
			t.Errorf("dues %s: stderr has no %q:\n%s", strings.Join(c.args, " "), c.line, r.Stderr)
		}
	}

	// The unencrypted-database note on stderr shows whether colour is on.
	note := func(args ...string) string {
		t.Helper()
		r := testutil.RunDues(t, bin, work, time.Minute, append(args, "list", "--dbpath", db)...)
		if r.ExitCode != 0 {
			t.Fatalf("list %v: exit %d\n%s", args, r.ExitCode, r.Combined())
		}
		return r.Stderr
	}
	if s := note("--color=always"); !strings.Contains(s, "\x1b[") {
		t.Errorf("--color=always: no colour on a piped stderr:\n%q", s)
	}
	// RunDues fails these if they write an escape code.
	note("--color=truecolor")
	note("--color=never")
	if s := note("--plain"); !strings.Contains(s, "dues: note: ") {
		t.Errorf("--plain: note not in plain form:\n%q", s)
	}

	r = duesRun(t, bin, work, "--color=sometimes", "list", "--dbpath", db)
	if r.ExitCode != 2 {
		t.Errorf("--color=sometimes: exit %d, want 2\n%s", r.ExitCode, r.Combined())
	}

	// The hidden extras print plain text on a pipe and never animate there.
	for _, c := range []struct{ cmd, want string }{
		{"coffee", "Forensic caffeine levels nominal.\n"},
		{"fortune", `"`},
		{"mew", " | #0042 |\n"},
	} {
		r := duesMust(t, bin, work, c.cmd)
		if !strings.Contains(r.Stdout, c.want) || r.Stderr != "" {
			t.Errorf("dues %s: stdout %q, stderr %q", c.cmd, r.Stdout, r.Stderr)
		}
	}
	help := duesMust(t, bin, work, "--help").Stdout
	for _, hidden := range []string{"coffee", "fortune", "mew"} {
		if strings.Contains(help, hidden) {
			t.Errorf("help mentions the hidden %q", hidden)
		}
	}

	// On a pipe, DUES names itself in one line: the whole of version and
	// --version, and the first line of help. BuildDues sets no commit.
	identity := regexp.MustCompile(`^DUES \d+\.\d+\.\d+(\+[0-9A-Za-z.-]+)? \([^)]*\) - Deduplicated Unified Evidence Store$`)
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		r := duesMust(t, bin, work, args...)
		if line, ok := strings.CutSuffix(r.Stdout, "\n"); !ok || !identity.MatchString(line) || r.Stderr != "" {
			t.Errorf("dues %s: stdout %q, stderr %q", strings.Join(args, " "), r.Stdout, r.Stderr)
		}
	}
	for _, args := range [][]string{{"--help"}, {"help"}, {}} {
		first, _, _ := strings.Cut(duesMust(t, bin, work, args...).Stdout, "\n")
		if !identity.MatchString(first) {
			t.Errorf("dues %s: help starts %q", strings.Join(args, " "), first)
		}
	}
}
