package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
// default to one; exports go to the database's folder by default, are kept
// a day, and wait five minutes for the uploads being stored.
func TestServerFlagsParse(t *testing.T) {
	t.Parallel()
	plain := newCLIDefinition()
	if _, err := plain.app.Parse([]string{"server"}); err != nil {
		t.Fatal(err)
	}
	if *plain.flags.serverUploadJobs != 1 {
		t.Errorf("upload jobs by default %d, want 1", *plain.flags.serverUploadJobs)
	}
	if f := plain.flags; *f.serverExportDir != "" || *f.serverExportKeep != server.DefaultExportKeepHours || *f.serverExportDrain != "5m0s" {
		t.Errorf("exports by default: folder %q, kept %d h, drain %q", *f.serverExportDir, *f.serverExportKeep, *f.serverExportDrain)
	}
	cliDef := newCLIDefinition()
	if _, err := cliDef.app.Parse([]string{"server", "--listen", "127.0.0.1:1", "--listen", "[::1]:2",
		"--cors-origin", "https://a.example", "--cors-origin", "http://b.example:8080", "--" + flagUploadJobs, "3",
		"--" + flagExportDir, "packages", "--" + flagExportKeepHours, "48", "--" + flagExportDrainTimeout, "90s"}); err != nil {
		t.Fatal(err)
	}
	if *cliDef.flags.serverUploadJobs != 3 {
		t.Errorf("upload jobs %d, want 3", *cliDef.flags.serverUploadJobs)
	}
	if f := cliDef.flags; *f.serverExportDir != "packages" || *f.serverExportKeep != 48 || *f.serverExportDrain != "90s" {
		t.Errorf("exports: folder %q, kept %d h, drain %q", *f.serverExportDir, *f.serverExportKeep, *f.serverExportDrain)
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

// TestServerExportFlags: the export settings reach the server checked, the
// folder made absolute; a value out of range is a usage error naming its
// flag, or, from the config file, its key and the file.
func TestServerExportFlags(t *testing.T) {
	t.Parallel()
	resolve := func(args ...string) (server.ExportConfig, error) {
		t.Helper()
		args = append([]string{"server"}, args...)
		cliDef := newCLIDefinition()
		if _, err := cliDef.app.Parse(args); err != nil {
			t.Fatal(err)
		}
		return resolveServerExports(cliDef.flags, cmdLineSources(t, cliDef, args))
	}
	def, err := resolve()
	if err != nil || def.Dir != "" || def.Keep != server.DefaultExportKeepHours*time.Hour || def.Drain != server.DefaultDrainTimeout || def.Creator != duesCreator() {
		t.Fatalf("defaults %+v (%v)", def, err)
	}
	got, err := resolve("--"+flagExportDir, "packages", "--"+flagExportKeepHours, "48", "--"+flagExportDrainTimeout, "90s")
	abs, aerr := filepath.Abs("packages")
	if err != nil || aerr != nil || got.Dir != abs || got.Keep != 48*time.Hour || got.Drain != 90*time.Second {
		t.Fatalf("given %+v (%v), want the folder %s", got, err, abs)
	}
	for _, c := range []struct{ flag, value string }{
		{flagExportKeepHours, "0"},
		{flagExportKeepHours, "721"},
		{flagExportDrainTimeout, "0s"},
		{flagExportDrainTimeout, "2h"},
		{flagExportDrainTimeout, "soon"},
	} {
		if _, err := resolve("--"+c.flag, c.value); lifecycle.ExitCode(err) != lifecycle.ExitUsage || !strings.HasPrefix(err.Error(), "--"+c.flag+": ") {
			t.Errorf("--%s %s: %v, want a usage error naming the flag", c.flag, c.value, err)
		}
	}

	dir := t.TempDir()
	f, src := filled(t, `{"server": {"export-dir": `+jsonPath(dir)+`, "export-keep-hours": 2, "export-drain-timeout": "10m"}}`, "server")
	if got, err := resolveServerExports(f, src); err != nil || got.Dir != dir || got.Keep != 2*time.Hour || got.Drain != 10*time.Minute {
		t.Errorf("from the config file: %+v (%v)", got, err)
	}
	file := writeConfig(t, `{"server": {"export-keep-hours": 0}}`)
	if _, _, err := readConfig([]string{"--config", file, "server"}); lifecycle.ExitCode(err) != lifecycle.ExitUsage ||
		!strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), `"server.`+flagExportKeepHours+`": `) {
		t.Errorf("a bad value in the config file: %v, want a usage error naming the file and the key", err)
	}
}
