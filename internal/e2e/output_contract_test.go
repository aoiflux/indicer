package e2e

import (
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"indicer/internal/testutil"
)

// TestOutputContract runs DUES with pipes, as scripts do, and holds it to
// its output contract (docs/CLI_UX.md): results on stdout, everything else
// on stderr; nothing decorated on a pipe; colour only when asked for.
// testutil.RunDuesEnv also fails any run that writes an escape code to a
// pipe without asking for colour.
func TestOutputContract(t *testing.T) {
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

	// The unencrypted-database note on stderr shows whether colour is on.
	note := func(env []string, args ...string) string {
		t.Helper()
		r := testutil.RunDuesEnv(t, bin, work, time.Minute, env, append(args, "list", "--dbpath", db)...)
		if r.ExitCode != 0 {
			t.Fatalf("list %v %v: exit %d\n%s", env, args, r.ExitCode, r.Combined())
		}
		return r.Stderr
	}
	if s := note(nil, "--color=always"); !strings.Contains(s, "\x1b[") {
		t.Errorf("--color=always: no colour on a piped stderr:\n%q", s)
	}
	if s := note([]string{"FORCE_COLOR=1"}); !strings.Contains(s, "\x1b[") {
		t.Errorf("FORCE_COLOR=1: no colour on a piped stderr:\n%q", s)
	}
	if s := note([]string{"FORCE_COLOR=1", "NO_COLOR=yes"}); strings.Contains(s, "\x1b") {
		t.Errorf("NO_COLOR=yes did not beat FORCE_COLOR:\n%q", s)
	}
	if s := note([]string{"FORCE_COLOR=1"}, "--color=never"); strings.Contains(s, "\x1b") {
		t.Errorf("--color=never did not beat FORCE_COLOR:\n%q", s)
	}
	if s := note(nil, "--plain"); !strings.Contains(s, "dues: note: ") {
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
}
