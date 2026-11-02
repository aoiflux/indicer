package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/lib/logging"
)

func TestMain(m *testing.M) {
	// No test reads the account's own config file.
	defaultConfigPath = func() (string, error) { return filepath.Join("testdata", "no-such-config.json"), nil }
	os.Exit(m.Run())
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "compression.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func jsonPath(t *testing.T, p string) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func withDefaultConfig(t *testing.T, p string) {
	prev := defaultConfigPath
	defaultConfigPath = func() (string, error) { return p, nil }
	t.Cleanup(func() { defaultConfigPath = prev })
}

// TestConfigFile: the file's log-dir and temp-dir apply when the command
// line gives neither, a flag wins over the file, and the sources say which
// chose each.
func TestConfigFile(t *testing.T) {
	logDir, temp, other := t.TempDir(), t.TempDir(), t.TempDir()
	good := writeConfig(t, `{"log-dir": `+jsonPath(t, logDir)+`, "temp-dir": `+jsonPath(t, temp)+`}`)

	g, rest, err := parseGlobal([]string{"--config", good, "capabilities"})
	if err != nil || g.logDir != logDir || g.tempDir != temp || !slices.Equal(rest, []string{"capabilities"}) {
		t.Fatalf("--config: %+v %q %v", g, rest, err)
	}
	if g.config.Path != good || !g.config.Chosen || g.from(flagLogDir) != logging.FromConfig || g.from(flagTempDir) != logging.FromConfig {
		t.Fatalf("--config sources: %+v", g)
	}

	g, _, err = parseGlobal([]string{"--config=" + good, "--temp-dir", other, "capabilities"})
	if err != nil || g.tempDir != other || g.from(flagTempDir) != logging.FromFlag || g.logDir != logDir {
		t.Fatalf("a flag over the file: %+v %v", g, err)
	}

	withDefaultConfig(t, good)
	g, _, err = parseGlobal([]string{"capabilities"})
	if err != nil || g.logDir != logDir || g.config.Path != good || g.config.Chosen {
		t.Fatalf("the default file: %+v %v", g, err)
	}
	g, _, err = parseGlobal([]string{"--no-config", "capabilities"})
	if err != nil || g.logDir != "" || g.tempDir != "" || g.config.None != "--no-config" || g.from(flagLogDir) != logging.FromDefault {
		t.Fatalf("--no-config: %+v %v", g, err)
	}
}

// TestConfigFileErrors: a file that cannot be used stops every command,
// -h included, with exit 2 and a message naming the file; a command-line
// mistake also prints the usage.
func TestConfigFileErrors(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "missing.json")
	good := writeConfig(t, `{}`)
	gone := filepath.Join(t.TempDir(), "gone")
	cases := []struct {
		name, config string // config is the default file's content; "" for none
		args         []string
		want         string
		usage        bool
	}{
		{name: "missing named file", args: []string{"--config", missing, "capabilities"}, want: "config file " + missing + ": "},
		{name: "empty --config", args: []string{"--config=", "capabilities"}, want: "--config needs a file", usage: true},
		{name: "both", args: []string{"--config", good, "--no-config", "capabilities"}, want: "--config and --no-config contradict each other; give one", usage: true},
		{name: "relative path", config: `{"log-dir": "logs"}`, args: []string{"capabilities"},
			want: `:1:13: "log-dir": "logs" is not an absolute path; the config file takes absolute paths only`},
		{name: "wrong case", config: `{"Temp-dir": "/t"}`, args: []string{"capabilities"},
			want: `:1:2: "Temp-dir": unknown key; keys are case-sensitive: did you mean "temp-dir"?`},
		{name: "a dues key", config: `{"dbpath": "/db"}`, args: []string{"capabilities"}, want: `"dbpath": unknown key`},
		{name: "hint", config: `{"log-dir": 1}`, args: []string{"capabilities"}, want: "(the default config file; compression --no-config runs without it)"},
		{name: "help too", config: `{"log-dir": 1}`, args: []string{"-h"}, want: `"log-dir": must be a string`},
		{name: "temp-dir from the file", config: `{"temp-dir": ` + jsonPath(t, gone) + `}`, args: []string{"capabilities"},
			want: "--temp-dir (set in config file "},
	}
	for _, c := range cases {
		withDefaultConfig(t, missing)
		if c.config != "" {
			withDefaultConfig(t, writeConfig(t, c.config))
		}
		code, env, stderr := runCLI(t, ctx, "", c.args...)
		if code != lifecycle.ExitUsage || env != nil || !strings.Contains(stderr, c.want) {
			t.Errorf("%s: exit %d, stdout %v, stderr %q; want exit 2 and %q", c.name, code, env, stderr, c.want)
		}
		if got := strings.Contains(stderr, "usage: compression"); got != c.usage {
			t.Errorf("%s: usage printed %v, want %v: %q", c.name, got, c.usage, stderr)
		}
	}

	withDefaultConfig(t, writeConfig(t, `{"log-dir": 1}`))
	if code, _, stderr := runCLI(t, ctx, "", "--no-config", "capabilities"); code != lifecycle.ExitOK {
		t.Errorf("--no-config past a broken default file: exit %d, %s", code, stderr)
	}
}
