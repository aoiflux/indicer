package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"

	coreerr "indicer/internal/core/errors"
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
		t.Fatalf("with DUES_LOG_DIR: %+v", cfg)
	}
	if cfg := logConfig(""); cfg.Dir != "" || !cfg.Lazy || cfg.MinLevel != zapcore.WarnLevel {
		t.Fatalf("without DUES_LOG_DIR: %+v", cfg)
	}
}

func TestDispatchAlwaysAnswers(t *testing.T) {
	d := platformapi.NewDefaultDispatcher()
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
	d := platformapi.NewDefaultDispatcher()
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
