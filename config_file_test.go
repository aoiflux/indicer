package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/internal/testutil/testdb"
	"indicer/internal/ui"
	"indicer/lib/cnst"
	"indicer/lib/logging"
)

// writeConfig writes a config file and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dues.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// jsonPath quotes a path for a config file.
func jsonPath(p string) string {
	b, _ := json.Marshal(p)
	return string(b)
}

// TestConfigKeysAreFlags: every key of the file is the long name of a flag
// of its section's command, or a global flag at the top level, so a
// renamed flag cannot leave a key behind; and every key has one binding,
// to a value of its own type.
func TestConfigKeysAreFlags(t *testing.T) {
	cliDef := newCLIDefinition()
	app := cliDef.app
	bound := map[string]binding{}
	for _, b := range cliDef.flags.bindings() {
		k := b.section + "." + b.flag
		if _, dup := bound[k]; dup {
			t.Errorf("%s is bound twice", k)
		}
		bound[k] = b
	}
	keys := 0
	flagsOf := func(cmd string) []string {
		var names []string
		model := app.Model().FlagGroupModel
		if cmd != "" {
			model = app.GetCommand(cmd).Model().FlagGroupModel
		}
		for _, f := range model.Flags {
			if !f.Hidden {
				names = append(names, f.Name)
			}
		}
		return names
	}
	var check func(typ reflect.Type, cmd string)
	check = func(typ reflect.Type, cmd string) {
		for sf := range typ.Fields() {
			key := sf.Tag.Get("json")
			if sf.Type.Elem().Kind() == reflect.Struct {
				check(sf.Type.Elem(), key)
				continue
			}
			if !slices.Contains(flagsOf(cmd), key) {
				t.Errorf("key %q is not a flag of %q", key, cmd)
			}
			keys++
			if b, ok := bound[cmd+"."+key]; !ok || reflect.TypeOf(b.dst) != sf.Type {
				t.Errorf("key %q of %q: binding %+v, want one to a %v", key, cmd, b, sf.Type)
			}
		}
	}
	check(reflect.TypeFor[fileConfig](), "")
	if keys != len(bound) {
		t.Errorf("%d keys, %d bindings", keys, len(bound))
	}
}

func TestConfigArgs(t *testing.T) {
	for _, c := range []struct {
		args        []string
		path        string
		given, off  bool
		rest        []string
		errContains string
	}{
		{args: []string{"list"}, rest: []string{"list"}},
		{args: []string{"--config", "/c.json", "list"}, path: "/c.json", given: true, rest: []string{"list"}},
		{args: []string{"list", "--config=/c.json", "-h"}, path: "/c.json", given: true, rest: []string{"list", "-h"}},
		{args: []string{"--no-config", "help"}, off: true, rest: []string{"help"}},
		{args: []string{"store", "--", "--config", "/c.json"}, rest: []string{"store", "--", "--config", "/c.json"}},
		{args: []string{"list", "--config"}, errContains: "--config needs a file"},
		{args: []string{"--config=", "list"}, errContains: "--config needs a file"},
		{args: []string{"--config", "/a", "--config", "/b"}, errContains: "given twice"},
		{args: []string{"--config", "/a", "--no-config", "list"}, path: "/a", given: true, off: true, rest: []string{"list"}},
	} {
		path, given, off, rest, err := configArgs(c.args)
		switch {
		case c.errContains != "":
			if lifecycle.ExitCode(err) != lifecycle.ExitUsage || !strings.Contains(err.Error(), c.errContains) {
				t.Errorf("%q: error %v, want a usage error containing %q", c.args, err, c.errContains)
			}
		case err != nil || path != c.path || given != c.given || off != c.off || !slices.Equal(rest, c.rest):
			t.Errorf("%q: got %q %v %v %q %v; want %q %v %v %q", c.args, path, given, off, rest, err, c.path, c.given, c.off, c.rest)
		}
	}
}

func TestReadConfigChoosesTheFile(t *testing.T) {
	good := writeConfig(t, `{"theme": "light"}`)
	broken := writeConfig(t, `{"theme": "purple"}`)
	missing := filepath.Join(t.TempDir(), "dues.json")
	withDefault := func(t *testing.T, p string, err error) {
		prev := defaultConfigPath
		defaultConfigPath = func() (string, error) { return p, err }
		t.Cleanup(func() { defaultConfigPath = prev })
	}

	t.Run("default file", func(t *testing.T) {
		withDefault(t, good, nil)
		c, _, err := readConfig([]string{"list"})
		if err != nil || c.Path != good || c.Chosen || *c.values.Theme != "light" {
			t.Fatalf("got %+v, %v", c, err)
		}
	})
	t.Run("no default file", func(t *testing.T) {
		withDefault(t, missing, nil)
		c, _, err := readConfig([]string{"list"})
		if err != nil || c.Path != "" || c.None != "no file at "+missing {
			t.Fatalf("got %+v, %v", c, err)
		}
	})
	t.Run("no home", func(t *testing.T) {
		withDefault(t, "", errors.New("uid 1000 has none"))
		c, _, err := readConfig([]string{"list"})
		if err != nil || c.Path != "" || !strings.Contains(c.None, "uid 1000 has none") {
			t.Fatalf("got %+v, %v", c, err)
		}
	})
	t.Run("broken default file", func(t *testing.T) {
		withDefault(t, broken, nil)
		_, _, err := readConfig([]string{"list"})
		if lifecycle.ExitCode(err) != lifecycle.ExitUsage || !strings.Contains(err.Error(), broken+":1:11: \"theme\": unknown value \"purple\"") ||
			!strings.Contains(err.Error(), "--no-config") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("no-config skips it", func(t *testing.T) {
		withDefault(t, broken, nil)
		c, _, err := readConfig([]string{"--no-config", "list"})
		if err != nil || c.Path != "" || c.None != "--no-config" || !c.Chosen {
			t.Fatalf("got %+v, %v", c, err)
		}
	})
	t.Run("config names one", func(t *testing.T) {
		withDefault(t, broken, nil)
		c, _, err := readConfig([]string{"--config", good, "list"})
		if err != nil || c.Path != good || !c.Chosen {
			t.Fatalf("got %+v, %v", c, err)
		}
	})
	t.Run("config and no-config contradict", func(t *testing.T) {
		withDefault(t, good, nil)
		_, _, err := readConfig([]string{"--config", good, "--no-config", "list"})
		if lifecycle.ExitCode(err) != lifecycle.ExitUsage || err.Error() != "--config and --no-config contradict each other; give one" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a missing named file is an error", func(t *testing.T) {
		withDefault(t, good, nil)
		_, _, err := readConfig([]string{"--config", missing, "list"})
		if lifecycle.ExitCode(err) != lifecycle.ExitUsage || !strings.HasPrefix(err.Error(), "config file "+missing+": ") ||
			strings.Contains(err.Error(), "--no-config") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestConfigValidate(t *testing.T) {
	abs := jsonPath(filepath.Join(t.TempDir(), "x"))
	good := `{
	"color": "never", "plain": true, "ci": false, "no-fun": true, "theme": "dark", "taskbar": true,
	"log-dir": ` + abs + `, "temp-dir": ` + abs + `, "debug": true, "dbpath": ` + abs + `, "password-file": ` + abs + `,
	"preset": "low-resource", "compress-level": "fast", "low": true,
	"store": {"pipeline": "staged", "hash-strategy": "async", "io-engine": "stdlib", "io-uring-queue-depth": 0, "store-workers": 4, "store-queue": 8},
	"restore": {"restore-workers": 2, "restore-queue": 4, "restore-buffer-mb": 64},
	"server": {"listen": ["127.0.0.1:0", "[::1]:9"], "token-file": ` + abs + `, "cors-origin": ["https://ui.example.org"]}
}`
	if _, _, err := readConfig([]string{"--config", writeConfig(t, good)}); err != nil {
		t.Fatalf("every key, good values: %v", err)
	}
	for _, c := range []struct{ in, key, msg string }{
		{`{"color": "auto "}`, "color", `unknown value "auto "`},
		{`{"color": "Always"}`, "color", "use auto, always, never, ansi, truecolor"},
		{`{"theme": "solarized"}`, "theme", "use auto, dark, light"},
		{`{"log-dir": "logs"}`, "log-dir", "is not an absolute path"},
		{`{"temp-dir": ""}`, "temp-dir", "is not an absolute path"},
		{`{"dbpath": "~/cases"}`, "dbpath", "is not an absolute path"},
		{`{"password-file": "pw.txt"}`, "password-file", "is not an absolute path"},
		{`{"preset": "turbo"}`, "preset", "use quick, performance or low-resource"},
		{`{"preset": ""}`, "preset", "unknown value"},
		{`{"compress-level": "max"}`, "compress-level", "use fast, default or best"},
		{`{"store": {"pipeline": "fast"}}`, "store.pipeline", "use batch-owner or staged"},
		{`{"store": {"hash-strategy": ""}}`, "store.hash-strategy", "unknown value"},
		{`{"store": {"io-engine": "aio"}}`, "store.io-engine", "use auto, stdlib or io-uring"},
		{`{"store": {"io-uring-queue-depth": -1}}`, "store.io-uring-queue-depth", "or more, got -1"},
		{`{"store": {"store-workers": -2}}`, "store.store-workers", "or more, got -2"},
		{`{"store": {"store-queue": -1}}`, "store.store-queue", "or more"},
		{`{"restore": {"restore-workers": -1}}`, "restore.restore-workers", "or more"},
		{`{"restore": {"restore-queue": -1}}`, "restore.restore-queue", "or more"},
		{`{"restore": {"restore-buffer-mb": -1}}`, "restore.restore-buffer-mb", "or more"},
		{`{"server": {"listen": ["localhost"]}}`, "server.listen", "want host:port"},
		{`{"server": {"token-file": "tok"}}`, "server.token-file", "is not an absolute path"},
		{`{"server": {"cors-origin": ["ui.example.org"]}}`, "server.cors-origin", "want scheme://host[:port]"},
		{`{"chonksize": 512}`, "chonksize", "unknown key"},
		{`{"password": "x"}`, "password", "unknown key"},
		{`{"store-workers": 4}`, "store-workers", `goes in the "store" section`},
	} {
		_, _, err := readConfig([]string{"--config", writeConfig(t, c.in)})
		if want := `"` + c.key + `": `; err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: got %v, want %q and %q", c.in, err, want, c.msg)
		}
	}

	// Every bad value is reported at once, each at its place.
	_, _, err := readConfig([]string{"--config", writeConfig(t, "{\n  \"theme\": \"x\",\n  \"log-dir\": \"y\"\n}")})
	if err == nil || !strings.Contains(err.Error(), `:2:12: "theme"`) || !strings.Contains(err.Error(), `:3:14: "log-dir"`) {
		t.Fatalf("got %v", err)
	}
}

// filled parses args with the file's settings filled in, as run does.
func filled(t *testing.T, file string, args ...string) (cliFlags, sources) {
	t.Helper()
	conf, _, err := readConfig(append([]string{"--config", writeConfig(t, file)}, args...))
	if err != nil {
		t.Fatal(err)
	}
	cliDef := newCLIDefinition()
	parsed, err := cliDef.app.Parse(args)
	if err != nil {
		t.Fatalf("parse %q: %v", args, err)
	}
	return cliDef.flags, conf.apply(cliDef.flags, parsed, cmdLineSources(t, cliDef, args).set)
}

// TestConfigPrecedence: the command line, then the file, then the default.
func TestConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	file := `{"theme": "light", "plain": true, "no-fun": true, "dbpath": ` + jsonPath(dir) + `, "debug": true}`

	f, s := filled(t, file, "list")
	if *f.theme != "light" || !*f.plain || !*f.noFun || *f.dbpath != dir || !*f.debug || *f.color != "auto" {
		t.Fatalf("from the file: theme %q plain %v no-fun %v dbpath %q debug %v color %q", *f.theme, *f.plain, *f.noFun, *f.dbpath, *f.debug, *f.color)
	}
	for name, want := range map[string]string{ui.FlagTheme: logging.FromConfig, cnst.FlagDBPath: logging.FromConfig, ui.FlagColor: logging.FromDefault} {
		if got := s.from(name); got != want {
			t.Errorf("%s from %s, want %s", name, got, want)
		}
	}

	f, s = filled(t, file, "--theme", "dark", "--no-plain", "--no-no-fun", "-d", "/other", "--no-debug", "list")
	if *f.theme != "dark" || *f.plain || *f.noFun || *f.dbpath != "/other" || *f.debug {
		t.Fatalf("flags over the file: theme %q plain %v no-fun %v dbpath %q debug %v", *f.theme, *f.plain, *f.noFun, *f.dbpath, *f.debug)
	}
	if s.from(ui.FlagTheme) != logging.FromFlag || len(s.filled) != 0 {
		t.Fatalf("sources %+v", s)
	}
}

// TestConfigTuningReachesTheTUI: the TUI stores and restores in process, so
// the file's store and restore sections tune it as they tune dues store and
// dues restore. The server section does not apply to it, and the store
// section applies to no command but store and the TUI.
func TestConfigTuningReachesTheTUI(t *testing.T) {
	testdb.Isolate(t)
	before := testdb.Snapshot()
	file := `{
	  "store": {"pipeline": "staged", "hash-strategy": "async", "io-engine": "stdlib",
	            "io-uring-queue-depth": 64, "store-workers": 3, "store-queue": 6},
	  "restore": {"restore-workers": 2, "restore-queue": 4, "restore-buffer-mb": 16},
	  "server": {"token-file": ` + jsonPath(filepath.Join(t.TempDir(), "token")) + `}
	}`
	run := func(file string, args ...string) (cliFlags, sources) {
		t.Helper()
		before.Restore()
		cliDef := newCLIDefinition()
		conf, _, err := readConfig(append([]string{"--config", writeConfig(t, file)}, args...))
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := cliDef.app.Parse(args)
		if err != nil {
			t.Fatal(err)
		}
		src := conf.apply(cliDef.flags, parsed, cmdLineSources(t, cliDef, args).set)
		if _, err := applyRuntimeConfig(parsed, cliDef, src); err != nil {
			t.Fatal(err)
		}
		return cliDef.flags, src
	}

	f, src := run(file, "tui")
	if cnst.StorePipelineMode != cnst.StorePipelineStaged || cnst.StoreHashStrategy != cnst.AsyncHashStrategy ||
		cnst.StoreIOEngine != cnst.StoreIOEngineStdlib || cnst.StoreIOUringQueueDepth != 64 ||
		cnst.StoreWorkerCount != 3 || cnst.StoreTaskQueueDepth != 6 {
		t.Errorf("the TUI's store: pipeline %q, hash strategy %q, io engine %q, io-uring depth %d, workers %d, queue %d",
			cnst.StorePipelineMode, cnst.StoreHashStrategy, cnst.StoreIOEngine, cnst.StoreIOUringQueueDepth, cnst.StoreWorkerCount, cnst.StoreTaskQueueDepth)
	}
	if cnst.RestoreWorkerCount != 2 || cnst.RestoreTaskQueueDepth != 4 || cnst.RestoreWriteBufferMB != 16 {
		t.Errorf("the TUI's restore: workers %d, queue %d, buffer %d MB", cnst.RestoreWorkerCount, cnst.RestoreTaskQueueDepth, cnst.RestoreWriteBufferMB)
	}
	for _, name := range []string{cnst.FlagStorePipeline, cnst.FlagStoreWorkers, cnst.FlagRestoreBufferMB} {
		if got := src.from(name); got != logging.FromConfig {
			t.Errorf("%s from %s, want %s", name, got, logging.FromConfig)
		}
	}
	if *f.serverTokenFile != "" || src.filled[flagTokenFile] {
		t.Error("the server section applied to the TUI")
	}

	// Without the sections, the TUI keeps the defaults it always had.
	run(`{}`, "tui")
	if cnst.StorePipelineMode != before.StorePipelineMode || cnst.StoreHashStrategy != before.StoreHashStrategy ||
		cnst.StoreIOEngine != before.StoreIOEngine || cnst.StoreIOUringQueueDepth != before.StoreIOUringQueueDepth {
		t.Errorf("an empty file changed the TUI's store: pipeline %q, hash strategy %q, io engine %q, io-uring depth %d",
			cnst.StorePipelineMode, cnst.StoreHashStrategy, cnst.StoreIOEngine, cnst.StoreIOUringQueueDepth)
	}

	// Another command is not tuned by them.
	_, src = run(file, "list")
	if cnst.StorePipelineMode != before.StorePipelineMode || cnst.StoreWorkerCount != before.StoreWorkerCount ||
		cnst.RestoreWorkerCount != before.RestoreWorkerCount || len(src.filled) != 0 {
		t.Errorf("list took the store or restore section: pipeline %q, workers %d, restore workers %d, filled %v",
			cnst.StorePipelineMode, cnst.StoreWorkerCount, cnst.RestoreWorkerCount, src.filled)
	}
}

// TestConfigPresets: the file's preset and its tuned settings count as
// explicit against each other, and give way together to a preset on the
// command line.
func TestConfigPresetRules(t *testing.T) {
	file := `{"preset": "quick", "compress-level": "best"}`
	for _, c := range []struct {
		args          []string
		preset, level string
		quick         bool
	}{
		// The file's level is explicit against the file's preset.
		{[]string{"list"}, cnst.PresetQuick, "best", true},
		// An explicit flag still wins.
		{[]string{"-z", "default", "list"}, cnst.PresetQuick, "default", true},
		// A preset on the command line replaces the file's preset and level.
		{[]string{"--performance-mode", "list"}, cnst.PresetPerformance, "best", false},
		{[]string{"--preset", "low-resource", "list"}, cnst.PresetLowResource, "default", false},
		{[]string{"--no-quick-mode", "list"}, "", "best", false},
	} {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			cliDef := newCLIDefinition()
			conf, _, err := readConfig(append([]string{"--config", writeConfig(t, file)}, c.args...))
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := cliDef.app.Parse(c.args)
			if err != nil {
				t.Fatal(err)
			}
			src := conf.apply(cliDef.flags, parsed, cmdLineSources(t, cliDef, c.args).set)
			tuning, err := applyRuntimeConfig(parsed, cliDef, src)
			if err != nil {
				t.Fatal(err)
			}
			if tuning.activePreset != c.preset || cnst.CompressLevel != c.level || cnst.QUICKOPT != c.quick {
				t.Fatalf("preset %q level %q quick %v; want %q %q %v", tuning.activePreset, cnst.CompressLevel, cnst.QUICKOPT, c.preset, c.level, c.quick)
			}
		})
	}
}

func TestConfigUnlockRule(t *testing.T) {
	pw := filepath.Join(t.TempDir(), "pw")
	other := filepath.Join(t.TempDir(), "other")
	file := `{"password-file": ` + jsonPath(pw) + `}`
	for _, c := range []struct {
		args []string
		want string
		from string
	}{
		{[]string{"list"}, pw, logging.FromConfig},
		{[]string{"--password-file", other, "list"}, other, logging.FromFlag},
		{[]string{"--password-file=", "list"}, "", logging.FromFlag},
		{[]string{"--password-stdin", "list"}, "", logging.FromDefault},
	} {
		f, s := filled(t, file, c.args...)
		if *f.pwdFile != c.want || s.from(flagPasswordFile) != c.from {
			t.Errorf("%q: password-file %q from %s; want %q from %s", c.args, *f.pwdFile, s.from(flagPasswordFile), c.want, c.from)
		}
	}
}

// TestConfigSections: a section applies to its own command only, and a
// list on the command line replaces the file's.
func TestConfigSections(t *testing.T) {
	file := `{"store": {"store-workers": 3, "pipeline": "staged"}, "restore": {"restore-workers": 5},
		"server": {"listen": ["127.0.0.1:7000", "[::1]:7000"], "cors-origin": ["https://a.example"]}}`

	f, _ := filled(t, file, "store", "ev.bin")
	if *f.storeWorkers != 3 || *f.storePipeline != "staged" || *f.restoreWorkers != 0 {
		t.Fatalf("store: workers %d pipeline %q restore workers %d", *f.storeWorkers, *f.storePipeline, *f.restoreWorkers)
	}
	f, _ = filled(t, file, "restore", "abc")
	if *f.restoreWorkers != 5 || *f.storeWorkers != 0 {
		t.Fatalf("restore: workers %d, store workers %d", *f.restoreWorkers, *f.storeWorkers)
	}
	f, _ = filled(t, file, "list")
	if *f.storeWorkers != 0 || *f.restoreWorkers != 0 {
		t.Fatal("a section applied to another command")
	}
	f, _ = filled(t, file, "server")
	if !slices.Equal(*f.serverListen, []string{"127.0.0.1:7000", "[::1]:7000"}) || !slices.Equal(*f.serverCORSOrigins, []string{"https://a.example"}) {
		t.Fatalf("server: %q %q", *f.serverListen, *f.serverCORSOrigins)
	}
	f, _ = filled(t, file, "server", "--listen", "127.0.0.1:8000")
	if !slices.Equal(*f.serverListen, []string{"127.0.0.1:8000"}) {
		t.Fatalf("the command line's list: %q", *f.serverListen)
	}
}

// TestConfigMessagesNameTheFile: a value from the file that a later check
// rejects says where it came from.
func TestConfigMessagesNameTheFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	p := writeConfig(t, `{"temp-dir": `+jsonPath(missing)+`, "store": {"store-workers": 100000}}`)
	_, logs := logArgs(t)
	for _, args := range [][]string{{"list"}, {"--temp-dir", t.TempDir(), "store", "ev.bin"}} {
		var code int
		_, stderr := captureOutput(t, func() { code = realMain(append(append(logs, "--config", p), args...)) })
		if code != lifecycle.ExitUsage || !strings.Contains(stderr, "(set in config file "+p+")") {
			t.Errorf("%q: exit %d, stderr:\n%s", args, code, stderr)
		}
	}
}

// TestBrokenConfigStopsEveryCommand: help and version too, with exit 2.
func TestBrokenConfigStopsEveryCommand(t *testing.T) {
	p := writeConfig(t, `{"theme": "light",}`)
	for _, args := range [][]string{{"help"}, {"--version"}, {"version"}, {"list"}, {}} {
		var code int
		stdout, stderr := captureOutput(t, func() { code = realMain(append([]string{"--config", p}, args...)) })
		if code != lifecycle.ExitUsage || stdout != "" || !strings.Contains(stderr, "config file "+p+":1:18: has a comma") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
	var code int
	stdout, _ := captureOutput(t, func() { code = realMain([]string{"--config", p, "--no-config", "help"}) })
	if code != lifecycle.ExitUsage {
		t.Errorf("--config with --no-config: exit %d, stdout %q", code, stdout)
	}
	stdout, _ = captureOutput(t, func() { code = realMain([]string{"--no-config", "help"}) })
	if code != lifecycle.ExitOK || !strings.HasPrefix(stdout, "DUES ") {
		t.Errorf("--no-config help: exit %d, stdout %q", code, stdout)
	}
}

// TestConfigReachesHelpAndTheLog: the file's presentation settings shape
// the help that prints before kingpin parses, and its log-dir holds the
// log.
func TestConfigReachesHelpAndTheLog(t *testing.T) {
	logDir := t.TempDir()
	p := writeConfig(t, `{"color": "always", "log-dir": `+jsonPath(logDir)+`}`)
	var code int
	stdout, _ := captureOutput(t, func() { code = realMain([]string{"--config", p, "--help"}) })
	if code != lifecycle.ExitOK || !strings.Contains(stdout, "\x1b[") {
		t.Fatalf("color always from the file: exit %d, help %q", code, stdout)
	}
	stdout, _ = captureOutput(t, func() { code = realMain([]string{"--config", p, "--color", "never", "--help"}) })
	if code != lifecycle.ExitOK || !strings.HasPrefix(stdout, "DUES ") || strings.Contains(stdout, "\x1b") {
		t.Fatalf("color never on the command line: exit %d, help %q", code, stdout)
	}

	captureOutput(t, func() { code = realMain([]string{"--config", p, "version"}) })
	rec := settingsRecord(t, logDir)
	if s, _ := rec[flagConfig].(map[string]any); s["value"] != p || s["source"] != logging.FromFlag {
		t.Errorf("config: %v", rec[flagConfig])
	}
	for name, from := range map[string]string{ui.FlagColor: logging.FromConfig, flagLogDir: logging.FromConfig, ui.FlagTheme: logging.FromDefault} {
		if s, _ := rec[name].(map[string]any); s["source"] != from {
			t.Errorf("%s: %v, want source %s", name, rec[name], from)
		}
	}
}

// TestConfigCommand: dues config prints one JSON document with every key,
// the value in effect and its source, and never reads the password file.
func TestConfigCommand(t *testing.T) {
	dir, logs := logArgs(t)
	pw := filepath.Join(t.TempDir(), "never-created")
	db := t.TempDir()
	p := writeConfig(t, `{"theme": "light", "dbpath": `+jsonPath(db)+`, "password-file": `+jsonPath(pw)+`,
		"store": {"store-workers": 6}, "server": {"listen": ["127.0.0.1:7000"]}}`)
	var code int
	stdout, stderr := captureOutput(t, func() { code = realMain(append(logs, "--config", p, "--plain", "config")) })
	if code != lifecycle.ExitOK || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	var doc struct {
		File     *string                    `json:"file"`
		Settings map[string]json.RawMessage `json:"settings"`
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil || dec.More() {
		t.Fatalf("stdout is not one JSON document (%v):\n%s", err, stdout)
	}
	if doc.File == nil || *doc.File != p {
		t.Fatalf("file %v, want %q", doc.File, p)
	}
	get := func(raw json.RawMessage) settingDoc {
		var s settingDoc
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	section := func(name string) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(doc.Settings[name], &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	for name, want := range map[string]settingDoc{
		ui.FlagTheme:     {Value: "light", Source: logging.FromConfig},
		ui.FlagPlain:     {Value: true, Source: logging.FromFlag},
		ui.FlagColor:     {Value: "auto", Source: logging.FromDefault},
		cnst.FlagDBPath:  {Value: db, Source: logging.FromConfig},
		flagLogDir:       {Value: dir, Source: logging.FromFlag},
		flagPasswordFile: {Value: pw, Source: logging.FromConfig},
	} {
		if got := get(doc.Settings[name]); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
	}
	for _, c := range []struct {
		section, key string
		want         settingDoc
	}{
		{"store", cnst.FlagStoreWorkers, settingDoc{Value: 6.0, Source: logging.FromConfig}},
		{"store", cnst.FlagStorePipeline, settingDoc{Value: cnst.StorePipelineBatch, Source: logging.FromDefault}},
		{"restore", cnst.FlagRestoreBufferMB, settingDoc{Value: 0.0, Source: logging.FromDefault}},
		{"server", flagListen, settingDoc{Value: []any{"127.0.0.1:7000"}, Source: logging.FromConfig}},
		{"server", flagCORSOrigin, settingDoc{Value: []any{}, Source: logging.FromDefault}},
	} {
		if got := get(section(c.section)[c.key]); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s.%s: %+v, want %+v", c.section, c.key, got, c.want)
		}
	}
	keys := len(doc.Settings) - 3 + len(section("store")) + len(section("restore")) + len(section("server"))
	if want := len(newCLIDefinition().flags.bindings()); keys != want {
		t.Errorf("%d settings, want one per key, %d", keys, want)
	}

	// No file: null, and why.
	stdout, _ = captureOutput(t, func() { code = realMain(append(logs, "--no-config", "config")) })
	if code != lifecycle.ExitOK || !strings.Contains(stdout, `"file": null,`) || !strings.Contains(stdout, `"reason": "--no-config"`) {
		t.Fatalf("exit %d:\n%s", code, stdout)
	}
}
