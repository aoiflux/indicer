package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"indicer/internal/testutil"
)

// TestConfigFile runs dues and compression with config files, black box:
// the file's settings apply, a flag wins over the file, dues config shows
// each value and its source, a broken file stops the command with exit 2
// before it writes anything, and a test's run reads no config file unless
// it names one.
func TestConfigFile(t *testing.T) {
	bin := testutil.BuildDues(t)
	work := t.TempDir()
	testutil.WriteRandomFile(t, work, "ev.bin", 3, 256<<10)
	db, logs := filepath.Join(work, "case"), t.TempDir()
	config := writeJSON(t, map[string]any{"dbpath": db, "debug": true, "log-dir": logs})

	// dues config: the values in effect and where each came from.
	var doc struct {
		File     string
		Settings map[string]json.RawMessage
	}
	if err := json.Unmarshal([]byte(duesMust(t, bin, work, "--config", config, "config").Stdout), &doc); err != nil {
		t.Fatalf("dues config: stdout is not one JSON document: %v", err)
	}
	if doc.File != config {
		t.Errorf("dues config: file %q, want %q", doc.File, config)
	}
	for key, want := range map[string]string{
		"dbpath":  `{"source":"config","value":` + jsonText(t, db) + `}`,
		"debug":   `{"source":"config","value":true}`,
		"log-dir": `{"source":"config","value":` + jsonText(t, logs) + `}`,
		"plain":   `{"source":"default","value":false}`,
	} {
		if got := sorted(t, doc.Settings[key]); got != want {
			t.Errorf("dues config: %s is %s, want %s", key, got, want)
		}
	}
	// The log went to the file's log-dir, at the file's debug level.
	rec := settingsRecords(t, logs)
	if len(rec) != 1 || source(rec[0], "config") != "flag" || source(rec[0], "dbpath") != "config" || source(rec[0], "debug") != "config" {
		t.Errorf("settings record in the file's log-dir: %v", rec)
	}
	if !strings.Contains(logText(t, logs), `"msg":"Command: config"`) {
		t.Errorf("no debug record in the file's log-dir")
	}

	// store and list use the file's database.
	duesMust(t, bin, work, "--config", config, "store", "ev.bin")
	list, err := parseList(duesMust(t, bin, work, "--config", config, "list").Stdout)
	if err != nil || len(list.Evidence) != 1 || filepath.Base(list.Evidence[0].Name) != "ev.bin" {
		t.Fatalf("list from the file's dbpath: %+v, %v", list, err)
	}
	if info, err := os.Stat(db); err != nil || !info.IsDir() {
		t.Fatalf("the file's dbpath %s: %v", db, err)
	}

	// A flag wins over the file.
	flagLogs := t.TempDir()
	duesMust(t, bin, work, "--config", config, "--log-dir", flagLogs, "version")
	if rec := settingsRecords(t, flagLogs); len(rec) != 1 || source(rec[0], "log-dir") != "flag" || source(rec[0], "dbpath") != "config" {
		t.Errorf("--log-dir over the file: %v", rec)
	}

	// A broken file stops the command before it writes anything.
	broken := writeJSON(t, map[string]any{"dbpath": "case2"})
	brokenLogs, db2 := t.TempDir(), filepath.Join(work, "db2")
	r := testutil.RunDues(t, bin, work, time.Minute, "--config", broken, "--log-dir", brokenLogs, "store", "ev.bin", "--dbpath", db2)
	if r.ExitCode != 2 || !strings.Contains(r.Stderr, broken+`:1:11: "dbpath": "case2" is not an absolute path`) || r.Stdout != "" {
		t.Errorf("a broken file: exit %d\n%s", r.ExitCode, r.Combined())
	}
	for _, p := range []string{db2, filepath.Join(work, "case2")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("a broken file: %s exists (%v)", p, err)
		}
	}
	if entries, _ := os.ReadDir(brokenLogs); len(entries) != 0 {
		t.Errorf("a broken file: the log directory holds %v", entries)
	}

	r = testutil.RunDues(t, bin, work, time.Minute, "--config", config, "--no-config", "version")
	if r.ExitCode != 2 || !strings.Contains(r.Stderr, "--config and --no-config contradict each other; give one") {
		t.Errorf("--config with --no-config: exit %d\n%s", r.ExitCode, r.Combined())
	}

	// The test runners read no config file unless the test names one.
	plainLogs := t.TempDir()
	duesMust(t, bin, work, "--log-dir", plainLogs, "version")
	if rec := settingsRecords(t, plainLogs); len(rec) != 1 || jsonText(t, rec[0]["config"]) != `{"reason":"--no-config","source":"flag","value":"none"}` {
		t.Errorf("a run with no --config: %v", rec)
	}

	// An argument that starts with @ is a file name, not a file of
	// arguments: ev.bin is not read as the command line.
	testutil.WriteRandomFile(t, work, "@ev.bin", 4, 64<<10)
	db3 := filepath.Join(work, "db3")
	duesMust(t, bin, work, "store", "@ev.bin", "--dbpath", db3)
	if list, err := parseList(duesMust(t, bin, work, "list", "--dbpath", db3).Stdout); err != nil ||
		len(list.Evidence) != 1 || filepath.Base(list.Evidence[0].Name) != "@ev.bin" {
		t.Errorf("store @ev.bin: %+v, %v", list, err)
	}

	// compression reads compression.json the same way.
	comp := testutil.BuildBinary(t, "./cmd/compression")
	compLogs := t.TempDir()
	compConfig := writeJSON(t, map[string]any{"log-dir": compLogs})
	if r := testutil.RunDues(t, comp, work, time.Minute, "--config", compConfig, "capabilities"); r.ExitCode != 0 {
		t.Fatalf("compression --config: exit %d\n%s", r.ExitCode, r.Combined())
	}
	if rec := settingsRecords(t, compLogs); len(rec) != 1 || source(rec[0], "log-dir") != "config" || source(rec[0], "config") != "flag" {
		t.Errorf("compression's settings record in the file's log-dir: %v", rec)
	}
	compBroken := writeJSON(t, map[string]any{"log-dir": 5})
	r = testutil.RunDues(t, comp, work, time.Minute, "--config", compBroken, "capabilities")
	if r.ExitCode != 2 || r.Stdout != "" || !strings.Contains(r.Stderr, compBroken+`:1:12: "log-dir": must be a string`) {
		t.Errorf("compression with a broken file: exit %d\n%s", r.ExitCode, r.Combined())
	}
}

func writeJSON(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func jsonText(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// sorted is the JSON value raw with its keys sorted, for comparing.
func sorted(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return jsonText(t, v)
}

func logText(t *testing.T, dir string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
	}
	return b.String()
}

// settingsRecords are the settings records in the logs in dir.
func settingsRecords(t *testing.T, dir string) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for line := range strings.SplitSeq(logText(t, dir), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["msg"] == "settings" {
			recs = append(recs, r)
		}
	}
	return recs
}

// source is where a settings record says a setting came from.
func source(rec map[string]any, name string) any {
	s, _ := rec[name].(map[string]any)
	return s["source"]
}
