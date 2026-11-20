package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/lib/dbmeta"
)

// credsFor parses args and resolves the password the way run does.
func credsFor(t *testing.T, stdin string, args ...string) (dbmeta.Credentials, error) {
	t.Helper()
	cliDef := newCLIDefinition()
	if _, err := cliDef.app.Parse(args); err != nil {
		t.Fatalf("parse %q: %v", args, err)
	}
	set, err := flagsSetByUser(cliDef.app, args)
	if err != nil {
		t.Fatal(err)
	}
	return resolveCredentials(cliDef.flags, sources{set: set}, strings.NewReader(stdin))
}

func TestPasswordSources(t *testing.T) {
	pwFile := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pwFile, []byte("from-file\r\nignored second line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// DUES_PASSWORD is not read: it was a source until the flags-only pass.
	t.Setenv("DUES_PASSWORD", "from-env")

	cases := []struct {
		name  string
		stdin string
		args  []string
		want  dbmeta.Credentials
	}{
		{"none", "", []string{"list"}, dbmeta.NoPassword()},
		{"stdin", "from-stdin\n", []string{"--password-stdin", "list"}, dbmeta.Password("from-stdin")},
		{"file beats stdin", "from-stdin\n", []string{"--password-file", pwFile, "--password-stdin", "list"}, dbmeta.Password("from-file")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := credsFor(t, c.stdin, c.args...)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(c.want) {
				t.Fatalf("credentials differ from %q", c.name)
			}
		})
	}
}

// TestPasswordFlagRemoved: --password and -p are refused, naming what
// replaces them, even with a file or stdin source given too.
func TestPasswordFlagRemoved(t *testing.T) {
	t.Parallel()
	pwFile := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pwFile, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-p", "x", "list"}, {"--password", "x", "list"}, {"--password=", "list"}, {"--password-file", pwFile, "-p", "x", "list"},
	} {
		_, err := credsFor(t, "", args...)
		if lifecycle.ExitCode(err) != lifecycle.ExitUsage || !strings.Contains(fmt.Sprint(err), "--password-file") {
			t.Errorf("%q: %v (exit %d); want a usage error naming --password-file", args, err, lifecycle.ExitCode(err))
		}
	}
}

func TestPasswordFileErrors(t *testing.T) {
	t.Parallel()
	_, err := credsFor(t, "", "--password-file", filepath.Join(t.TempDir(), "missing"), "list")
	if err == nil || lifecycle.ExitCode(err) != lifecycle.ExitUsage {
		t.Fatalf("missing password file: %v (exit %d)", err, lifecycle.ExitCode(err))
	}
	if _, err := credsFor(t, "", "--password-stdin", "list"); err == nil {
		t.Fatal("empty stdin accepted as a password")
	}
}

// TestStdinSecretsExclusive: only the first line of standard input is a
// secret, so two flags that each read one from it are refused; the
// package's password comes from its own file or stdin flag.
func TestStdinSecretsExclusive(t *testing.T) {
	stdinFlags := []string{"--password-stdin", "--actor-key-password-stdin", "--source-password-stdin"}
	for i, a := range stdinFlags {
		for _, b := range stdinFlags[i+1:] {
			cliDef := newCLIDefinition()
			args := []string{"package", "merge", "p.duesarc", a, b}
			if _, err := cliDef.app.Parse(args); err != nil {
				t.Fatalf("parse %q: %v", args, err)
			}
			if err := checkOneStdin(cliDef.flags); lifecycle.ExitCode(err) != lifecycle.ExitUsage {
				t.Errorf("%s with %s: %v", a, b, err)
			}
		}
	}

	pwFile := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pwFile, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		stdin string
		args  []string
		want  dbmeta.Credentials
		usage bool
	}{
		{"", nil, dbmeta.NoPassword(), false},
		{"", []string{"--source-password-file", pwFile}, dbmeta.Password("from-file"), false},
		{"from-stdin\n", []string{"--source-password-stdin"}, dbmeta.Password("from-stdin"), false},
		{"", []string{"--source-password-stdin"}, dbmeta.Credentials{}, true},
		{"x\n", []string{"--source-password-file", pwFile, "--source-password-stdin"}, dbmeta.Credentials{}, true},
	} {
		cliDef := newCLIDefinition()
		args := append([]string{"package", "merge", "p.duesarc"}, c.args...)
		if _, err := cliDef.app.Parse(args); err != nil {
			t.Fatalf("parse %q: %v", args, err)
		}
		got, err := resolveSourceCredentials(cliDef.flags, strings.NewReader(c.stdin))
		if c.usage {
			if lifecycle.ExitCode(err) != lifecycle.ExitUsage {
				t.Errorf("%q: %v, want a usage error", c.args, err)
			}
			continue
		}
		if err != nil || !got.Equal(c.want) {
			t.Errorf("%q: %v", c.args, err)
		}
	}
}
