package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/api"
	"indicer/cli"
	"indicer/internal/lifecycle"
)

// TestServerTokenSources: --token-file, then DUES_TOKEN, then --token, which
// warns that the command line is visible.
func TestServerTokenSources(t *testing.T) {
	tokFile := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tokFile, []byte("from-file-0123456789abcdef0123456789\r\nignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		env     string
		args    []string
		want    string
		warning bool
		usage   bool
	}{
		{"none", "", nil, "", false, false},
		{"flag", "", []string{"--token", "from-flag"}, "from-flag", true, false},
		{"env over flag", "from-env", []string{"--token", "from-flag"}, "from-env", false, false},
		{"file over env", "from-env", []string{"--token-file", tokFile}, "from-file-0123456789abcdef0123456789", false, false},
		{"missing file", "", []string{"--token-file", filepath.Join(t.TempDir(), "nope")}, "", false, true},
		{"empty file", "", []string{"--token-file", empty}, "", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(envToken, c.env)
			cliDef := newCLIDefinition()
			if _, err := cliDef.app.Parse(append([]string{"server"}, c.args...)); err != nil {
				t.Fatal(err)
			}
			tok, warnings, err := resolveServerToken(cliDef.flags)
			if c.usage {
				if lifecycle.ExitCode(err) != lifecycle.ExitUsage {
					t.Fatalf("error %v, want a usage error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tok != c.want || (len(warnings) > 0) != c.warning {
				t.Fatalf("token %q, warnings %q; want %q, warning %v", tok, warnings, c.want, c.warning)
			}
		})
	}
}

// TestServerFlagsParse: the listen and origin flags repeat.
func TestServerFlagsParse(t *testing.T) {
	cliDef := newCLIDefinition()
	if _, err := cliDef.app.Parse([]string{"server", "--listen", "127.0.0.1:1", "--listen", "[::1]:2",
		"--cors-origin", "https://a.example", "--cors-origin", "http://b.example:8080"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*cliDef.flags.serverListen, " "); got != "127.0.0.1:1 [::1]:2" {
		t.Errorf("listen %q", got)
	}
	if got := strings.Join(*cliDef.flags.serverCORSOrigins, " "); got != "https://a.example http://b.example:8080" {
		t.Errorf("origins %q", got)
	}
}

// TestServerRefusesExposureWithoutToken: a non-loopback address without a
// token is a usage error, found before the database is opened or created.
func TestServerRefusesExposureWithoutToken(t *testing.T) {
	root := filepath.Join(t.TempDir(), "db")
	err := api.Server(context.Background(), cli.DBOptions{Path: root}, api.Options{Listen: []string{"0.0.0.0:0"}})
	if lifecycle.ExitCode(err) != lifecycle.ExitUsage || !strings.Contains(err.Error(), "token") {
		t.Fatalf("error %v (exit %d), want a usage error naming the token", err, lifecycle.ExitCode(err))
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("the database was created: %v", err)
	}
}
