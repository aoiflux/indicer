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
	"indicer/lib/server"
)

// TestServerTokenSources: the tokens come from --token-file only. DUES_TOKEN
// is not read, and the removed --token is refused, naming --token-file.
func TestServerTokenSources(t *testing.T) {
	t.Setenv("DUES_TOKEN", "from-env")
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const tok = "from-file-0123456789abcdef0123456789"
	one := write("one", tok+"\r\n")
	twoLines := write("two-lines", tok+"\r\nignored\n")
	v1 := write("v1", server.TokenFileHeader+"\n"+tok+" read reader\n"+tok+"x all admin\n")
	empty := write("empty", "\n")
	for _, c := range []struct {
		name  string
		args  []string
		want  string // label:scopes of each token
		usage string // part of the usage error expected; empty for none
	}{
		{"none", nil, "", ""},
		{"file", []string{"--token-file", one}, "default:read,upload", ""},
		{"v1 file", []string{"--token-file", v1}, "reader:read admin:read,upload,export", ""},
		{"second line", []string{"--token-file", twoLines}, "", server.TokenFileHeader},
		{"removed flag", []string{"--token", "from-flag"}, "", "--token-file"},
		{"removed flag with a file", []string{"--token-file", one, "--token", "x"}, "", "--token-file"},
		{"missing file", []string{"--token-file", filepath.Join(dir, "nope")}, "", "--token-file"},
		{"directory", []string{"--token-file", dir}, "", "is a directory"},
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
			creds, err := resolveServerTokens(cliDef.flags, sources{set: set})
			if c.usage != "" {
				if lifecycle.ExitCode(err) != lifecycle.ExitUsage || !strings.Contains(err.Error(), c.usage) || strings.Contains(err.Error(), tok) {
					t.Fatalf("error %v, want a usage error containing %q and not the token", err, c.usage)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, cr := range creds {
				got = append(got, cr.Label()+":"+cr.Scopes().String())
			}
			if strings.Join(got, " ") != c.want {
				t.Fatalf("tokens %q, want %q", got, c.want)
			}
		})
	}
}

// TestServerFlagsParse: the listen and origin flags repeat; upload jobs
// default to one.
func TestServerFlagsParse(t *testing.T) {
	t.Parallel()
	plain := newCLIDefinition()
	if _, err := plain.app.Parse([]string{"server"}); err != nil {
		t.Fatal(err)
	}
	if *plain.flags.serverUploadJobs != 1 {
		t.Errorf("upload jobs by default %d, want 1", *plain.flags.serverUploadJobs)
	}
	cliDef := newCLIDefinition()
	if _, err := cliDef.app.Parse([]string{"server", "--listen", "127.0.0.1:1", "--listen", "[::1]:2",
		"--cors-origin", "https://a.example", "--cors-origin", "http://b.example:8080", "--" + flagUploadJobs, "3"}); err != nil {
		t.Fatal(err)
	}
	if *cliDef.flags.serverUploadJobs != 3 {
		t.Errorf("upload jobs %d, want 3", *cliDef.flags.serverUploadJobs)
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
