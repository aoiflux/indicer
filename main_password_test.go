package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/lib/dbmeta"
)

// credsFor parses args and resolves the password the way run does.
func credsFor(t *testing.T, stdin string, args ...string) (dbmeta.Credentials, []string, error) {
	t.Helper()
	cliDef := newCLIDefinition()
	if _, err := cliDef.app.Parse(args); err != nil {
		t.Fatalf("parse %q: %v", args, err)
	}
	set, err := flagsSetByUser(cliDef.app, args)
	if err != nil {
		t.Fatal(err)
	}
	return resolveCredentials(cliDef.flags, set, strings.NewReader(stdin))
}

func TestPasswordSources(t *testing.T) {
	pwFile := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pwFile, []byte("from-file\r\nignored second line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envPassword, "")

	cases := []struct {
		name    string
		env     string
		stdin   string
		args    []string
		want    dbmeta.Credentials
		warning bool
	}{
		{"none", "", "", []string{"list"}, dbmeta.NoPassword(), false},
		{"argv warns", "", "", []string{"-p", "from-argv", "list"}, dbmeta.Password("from-argv"), true},
		{"env", "from-env", "", []string{"list"}, dbmeta.Password("from-env"), false},
		{"env beats argv", "from-env", "", []string{"-p", "from-argv", "list"}, dbmeta.Password("from-env"), false},
		{"stdin", "from-env", "from-stdin\n", []string{"--password-stdin", "list"}, dbmeta.Password("from-stdin"), false},
		{"file beats all", "from-env", "from-stdin\n", []string{"--password-file", pwFile, "--password-stdin", "-p", "x", "list"}, dbmeta.Password("from-file"), false},
		// An empty password is no password: a new database is unencrypted and
		// a legacy one opens with the empty-password key either way.
		{"empty argv", "", "", []string{"-p", "", "list"}, dbmeta.NoPassword(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(envPassword, c.env)
			got, warnings, err := credsFor(t, c.stdin, c.args...)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(c.want) {
				t.Fatalf("credentials differ from %q", c.name)
			}
			if (len(warnings) > 0) != c.warning {
				t.Fatalf("warnings %q, want warning=%v", warnings, c.warning)
			}
		})
	}
}

func TestPasswordFileErrors(t *testing.T) {
	t.Setenv(envPassword, "")
	_, _, err := credsFor(t, "", "--password-file", filepath.Join(t.TempDir(), "missing"), "list")
	if err == nil || lifecycle.ExitCode(err) != lifecycle.ExitUsage {
		t.Fatalf("missing password file: %v (exit %d)", err, lifecycle.ExitCode(err))
	}
	if _, _, err := credsFor(t, "", "--password-stdin", "list"); err == nil {
		t.Fatal("empty stdin accepted as a password")
	}
}
