package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"

	coreerr "indicer/internal/core/errors"
	"indicer/lib/logging"
	platformapi "indicer/pkg/api"
)

type envelope struct {
	Status string          `json:"status"`
	Error  *coreerr.Error  `json:"error"`
	Result json.RawMessage `json:"result"`
}

func decode(t *testing.T, out string) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal([]byte(out), &env); err != nil || env.Status == "" {
		t.Fatalf("not a response envelope: %q (%v)", out, err)
	}
	return env
}

func TestLogConfig(t *testing.T) {
	if cfg := logConfig("/some/dir"); cfg.Dir != "/some/dir" || cfg.Lazy || cfg.MinLevel != zapcore.InfoLevel {
		t.Fatalf("with a log directory: %+v", cfg)
	}
	if cfg := logConfig(""); cfg.Dir != "" || !cfg.Lazy || cfg.MinLevel != zapcore.WarnLevel {
		t.Fatalf("without one: %+v", cfg)
	}
}

func TestParseConfig(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(dir, "logs")
	good := map[string]libConfig{
		`{}`:   {},
		`null`: {},
		`{"log_dir":` + quote(logs) + `,"temp_dir":` + quote(dir) + `}`: {LogDir: logs, TempDir: dir},
	}
	for doc, want := range good {
		if got, err := parseConfig(doc); err != nil || got != want {
			t.Errorf("%s: %+v, %v; want %+v", doc, got, err, want)
		}
	}
	for _, doc := range []string{
		``, `[]`, `"x"`, `{} {}`, `{"logdir":"x"}`, `{"log_dir":5}`,
		`{"log_dir":"relative"}`, `{"temp_dir":"relative"}`,
		`{"temp_dir":` + quote(filepath.Join(dir, "missing")) + `}`,
		`{"temp_dir":` + quote(file) + `}`,
	} {
		if _, err := parseConfig(doc); err == nil || err.Code != coreerr.CodeInvalidRequest || !strings.HasPrefix(err.Message, "configuration: ") {
			t.Errorf("%s: %v, want an invalid_request about the configuration", doc, err)
		}
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// resetLib forgets the library's configuration, for a test that sets it up,
// and closes the log the test started.
func resetLib(t *testing.T) {
	t.Helper()
	lib.Lock()
	lib.configured, lib.dispatcher = false, nil
	lib.Unlock()
	t.Cleanup(func() {
		lib.Lock()
		lib.configured, lib.dispatcher = false, nil
		lib.Unlock()
		_ = logging.Close()
	})
}

func TestConfigureOnceBeforeRequests(t *testing.T) {
	resetLib(t)
	logs, temp := t.TempDir(), t.TempDir()
	env := decode(t, configure(`{"log_dir":`+quote(logs)+`,"temp_dir":`+quote(temp)+`}`))
	var res map[string]string
	if err := json.Unmarshal(env.Result, &res); env.Status != "ok" || err != nil || res["log_dir"] != logs || res["temp_dir"] != temp {
		t.Fatalf("configure: %+v (%v)", env, err)
	}
	d := lib.dispatcher

	env = decode(t, configure(`{"temp_dir":`+quote(t.TempDir())+`}`))
	if env.Error == nil || env.Error.Code != coreerr.CodeInvalidRequest || env.Error.Details["reason"] != "already_configured" {
		t.Fatalf("second configure: %+v", env)
	}
	if lib.dispatcher != d {
		t.Fatal("a refused configure replaced the dispatcher")
	}

	_ = logging.Sync()
	b, err := os.ReadFile(logging.Current().Path)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	for line := range strings.SplitSeq(string(b), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["msg"] == "settings" {
			settings = r
		}
	}
	for _, name := range []string{"log_dir", "temp_dir"} {
		if s, _ := settings[name].(map[string]any); s["source"] != logging.FromConfig {
			t.Errorf("settings %s: %v, want it from the configuration", name, settings[name])
		}
	}

	resetLib(t)
	if env := decode(t, dispatch(dispatcher(), `{"version":"v1","product":"qwikpak","operation":"capabilities"}`)); env.Status != "ok" {
		t.Fatalf("capabilities: %+v", env)
	}
	env = decode(t, configure(`{}`))
	if env.Error == nil || env.Error.Details["reason"] != "requests_started" {
		t.Fatalf("configure after a request: %+v", env)
	}
}

func TestDispatchAlwaysAnswers(t *testing.T) {
	d := platformapi.NewDefaultDispatcher(platformapi.Options{})
	for _, req := range []string{"", "{", "null", `{"version":"v1"}`, emptyRequestJSON} {
		if env := decode(t, dispatch(d, req)); env.Status != "error" {
			t.Fatalf("%q: %+v", req, env)
		}
	}
	env := decode(t, dispatch(d, `{"version":"v1","product":"qwikpak","operation":"capabilities"}`))
	if env.Status != "ok" {
		t.Fatalf("capabilities: %+v", env)
	}
}

func TestShutdownRefusesLaterRequests(t *testing.T) {
	d := platformapi.NewDefaultDispatcher(platformapi.Options{})
	start := time.Now()
	env := decode(t, shutdown(d, 0))
	if env.Status != "ok" || time.Since(start) > 5*time.Second {
		t.Fatalf("shutdown: %+v after %v", env, time.Since(start))
	}
	var res platformapi.ShutdownResult
	if err := json.Unmarshal(env.Result, &res); err != nil || res.RunningTasks != 0 {
		t.Fatalf("shutdown result %s (%v)", env.Result, err)
	}
	env = decode(t, d.DispatchJSON(context.Background(), `{"version":"v1","product":"qwikpak","operation":"capabilities"}`))
	if env.Error == nil || env.Error.Code != coreerr.CodeCanceled || env.Error.Details["reason"] != "shutdown" {
		t.Fatalf("request after shutdown: %+v", env)
	}
}
