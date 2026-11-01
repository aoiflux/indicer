package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/internal/ui"
	"indicer/lib/cnst"
	"indicer/lib/logging"
)

// TestSettingsRecord: a run logs one settings record, with the settings in
// effect and where each came from, and never the password.
func TestSettingsRecord(t *testing.T) {
	dir, logs := logArgs(t)
	temp := t.TempDir()
	pw := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pw, []byte("hunter2-s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var code int
	captureOutput(t, func() {
		code = realMain(append(logs, "--theme", "light", "--no-fun", "--temp-dir", temp, "--password-file", pw, "version"))
	})
	if code != lifecycle.ExitOK {
		t.Fatalf("exited %d, want 0", code)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "dues-*.ndjson"))
	var raw []byte
	var rec map[string]any
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, b...)
		for line := range strings.SplitSeq(string(b), "\n") {
			var r map[string]any
			if json.Unmarshal([]byte(line), &r) != nil || r["msg"] != "settings" {
				continue
			}
			if rec != nil {
				t.Fatalf("two settings records:\n%s", b)
			}
			rec = r
		}
	}
	if rec == nil {
		t.Fatalf("no settings record in %v", files)
	}
	if bytes.Contains(raw, []byte("hunter2-s3cret")) {
		t.Fatalf("the password reached the log:\n%s", raw)
	}

	want := map[string][2]any{
		ui.FlagTheme: {"light", logging.FromFlag},
		ui.FlagNoFun: {true, logging.FromFlag},
		ui.FlagColor: {"auto", logging.FromDefault},
		flagDebug:    {false, logging.FromDefault},
		flagTempDir:  {temp, logging.FromFlag},
		"unlock":     {flagPasswordFile, logging.FromFlag},
	}
	for name, w := range want {
		if s, _ := rec[name].(map[string]any); s["value"] != w[0] || s["source"] != w[1] {
			t.Errorf("%s: %v, want value %v from %v", name, rec[name], w[0], w[1])
		}
	}
	if s, _ := rec[flagLogDir].(map[string]any); s["source"] != logging.FromFlag || s["given"] != nil {
		t.Errorf("%s: %v, want the flag's directory in effect", flagLogDir, rec[flagLogDir])
	}
	if s, _ := rec[cnst.FlagDBPath].(map[string]any); s["source"] != logging.FromDefault || s["value"] == "" {
		t.Errorf("%s: %v, want the default root", cnst.FlagDBPath, rec[cnst.FlagDBPath])
	}
}
