package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/api"
	"indicer/internal/lifecycle"
	"indicer/lib/dbopen"
)

// TestServerTokenSources: the token comes from --token-file only. DUES_TOKEN
// is not read, and the removed --token is refused, naming --token-file.
func TestServerTokenSources(t *testing.T) {
	t.Setenv("DUES_TOKEN", "from-env")
	tokFile := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tokFile, []byte("from-file-0123456789abcdef0123456789\r\nignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		args  []string
		want  string
		usage string // part of the usage error expected; empty for none
	}{
		{"none", nil, "", ""},
		{"file", []string{"--token-file", tokFile}, "from-file-0123456789abcdef0123456789", ""},
		{"removed flag", []string{"--token", "from-flag"}, "", "--token-file"},
		{"removed flag with a file", []string{"--token-file", tokFile, "--token", "x"}, "", "--token-file"},
		{"missing file", []string{"--token-file", filepath.Join(t.TempDir(), "nope")}, "", "--token-file"},
		{"empty file", []string{"--token-file", empty}, "", "holds no token"},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"server"}, c.args...)
			cliDef := newCLIDefinition()
			if _, err := cliDef.app.Parse(args); err != nil {
				t.Fatal(err)
			}
			set, err := flagsSetByUser(cliDef.app, args)
			if err != nil {
				t.Fatal(err)
			}
			tok, err := resolveServerToken(cliDef.flags, sources{set: set})
			if c.usage != "" {
				if lifecycle.ExitCode(err) != lifecycle.ExitUsage || !strings.Contains(err.Error(), c.usage) {
					t.Fatalf("error %v, want a usage error containing %q", err, c.usage)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tok != c.want {
				t.Fatalf("token %q, want %q", tok, c.want)
			}
		})
	}
}

// TestServerFlagsParse: the listen and origin flags repeat.
func TestServerFlagsParse(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	root := filepath.Join(t.TempDir(), "db")
	err := api.Server(context.Background(), dbopen.Request{Path: root}, api.Options{Listen: []string{"0.0.0.0:0"}})
	if lifecycle.ExitCode(err) != lifecycle.ExitUsage || !strings.Contains(err.Error(), "token") {
		t.Fatalf("error %v (exit %d), want a usage error naming the token", err, lifecycle.ExitCode(err))
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("the database was created: %v", err)
	}
}
