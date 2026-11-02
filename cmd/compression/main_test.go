package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/lib/logging"
)

// run is realMain without the process setup: no log, no signal handling.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) int {
	g, rest, err := parseGlobal(args)
	return execute(ctx, g, rest, err, stdout, stderr, stdin)
}

func runCLI(t *testing.T, ctx context.Context, stdin string, args ...string) (int, map[string]any, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(ctx, args, &out, &errb, strings.NewReader(stdin))
	var env map[string]any
	if out.Len() > 0 {
		if err := json.Unmarshal(out.Bytes(), &env); err != nil {
			t.Fatalf("stdout is not one JSON envelope: %q", out.String())
		}
	}
	return code, env, errb.String()
}

func TestExitCodes(t *testing.T) {
	interrupted, cancel := context.WithCancelCause(context.Background())
	cancel(&lifecycle.SignalError{Signal: os.Interrupt})
	for _, c := range []struct {
		ctx  context.Context
		out  string
		want int
	}{
		{context.Background(), `{"status":"ok"}`, 0},
		{context.Background(), `{"status":"error","error":{"code":"invalid_request"}}`, 2},
		{context.Background(), `{"status":"error","error":{"code":"unknown_operation"}}`, 2},
		{context.Background(), `{"status":"error","error":{"code":"invalid_archive"}}`, 1},
		{context.Background(), `{"status":"error","error":{"code":"limit_exceeded"}}`, 1},
		{context.Background(), `{"status":"error","error":{"code":"internal_error"}}`, 1},
		{context.Background(), `{"status":"error","error":{"code":"internal_error","details":{"panic_id":"ab12"}}}`, 70},
		{context.Background(), `{"status":"error","error":{"code":"canceled"}}`, 1},
		{interrupted, `{"status":"error","error":{"code":"canceled"}}`, 130},
		{context.Background(), `not json`, 70},
	} {
		if got := exitCode(c.ctx, c.out); got != c.want {
			t.Errorf("%s: exit %d, want %d", c.out, got, c.want)
		}
	}
}

// TestErrorExitsNonZero is the CMP-5 regression: a failed request exited 0.
func TestErrorExitsNonZero(t *testing.T) {
	code, env, _ := runCLI(t, context.Background(), "", cmdListArchive, "--input", filepath.Join(t.TempDir(), "missing.duesarc"))
	if code == 0 || env["status"] != "error" {
		t.Fatalf("exit %d, %v", code, env)
	}
}

func TestAsyncCommandsRemoved(t *testing.T) {
	if code, _, _ := runCLI(t, context.Background(), "", "get-progress", "--task-id", "x"); code != 2 {
		t.Fatalf("get-progress: exit %d", code)
	}
	if code, _, _ := runCLI(t, context.Background(), "", "cancel-task", "--task-id", "x"); code != 2 {
		t.Fatalf("cancel-task: exit %d", code)
	}
	if code, _, _ := runCLI(t, context.Background(), "", cmdCreateArchive, "--input", ".", "--async"); code != 2 {
		t.Fatalf("--async: exit %d", code)
	}
}

func TestPasswordSources(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "pw")
	if err := os.WriteFile(file, []byte("from-file\r\nignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// DUES_PASSWORD is not read: it was a source until the flags-only pass.
	t.Setenv("DUES_PASSWORD", "from-env")
	cases := []struct {
		file    string
		stdin   bool
		removed string
		want    string
		err     error
	}{
		{file: file, stdin: true, want: "from-file"},
		{stdin: true, want: "from-stdin"},
		{want: ""},
		{file: file, removed: "x", err: errPasswordRemoved},
	}
	for _, c := range cases {
		var errb bytes.Buffer
		r := &runner{stderr: &errb, stdin: strings.NewReader("from-stdin\nmore")}
		got, err := r.password(passwordFlags{file: &c.file, stdin: &c.stdin, removed: &c.removed})
		if !errors.Is(err, c.err) || got != c.want {
			t.Errorf("%+v: %q, %v", c, got, err)
		}
	}
}

func TestArchiveCommands(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "case")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"a.txt": "alpha", "sub/b.txt": "beta", "sub/c.txt": "alpha"} {
		if err := os.WriteFile(filepath.Join(src, filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pw := filepath.Join(dir, "pw")
	if err := os.WriteFile(pw, []byte("s3cret-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "case.duesarc")
	ctx := context.Background()
	if code, env, stderr := runCLI(t, ctx, "", cmdCreateArchive, "--input", src, "--output", archive, "--password-file", pw); code != 0 {
		t.Fatalf("create-archive: exit %d %v %s", code, env, stderr)
	}
	if code, env, _ := runCLI(t, ctx, "", cmdVerifyArchive, "--input", archive); code != 2 {
		t.Fatalf("verify without a password: exit %d %v", code, env)
	}
	code, env, _ := runCLI(t, ctx, "s3cret-pass\n", cmdVerifyArchive, "--input", archive, "--password-stdin")
	result, _ := env["result"].(map[string]any)
	if code != 0 || result["valid"] != true {
		t.Fatalf("verify: exit %d %v", code, env)
	}
	if code, _, _ := runCLI(t, ctx, "wrong\n", cmdExtractArchive, "--input", archive, "--output", filepath.Join(dir, "x"), "--password-stdin"); code != 2 {
		t.Fatalf("extract with a wrong password: exit %d", code)
	}
	out := filepath.Join(dir, "out")
	if code, env, _ := runCLI(t, ctx, "", cmdExtractArchive, "--input", archive, "--output", out, "--password-file", pw); code != 0 {
		t.Fatalf("extract: exit %d %v", code, env)
	}
	for name, want := range map[string]string{"a.txt": "alpha", "sub/b.txt": "beta", "sub/c.txt": "alpha"} {
		if got, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(name))); err != nil || string(got) != want {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	if code, env, _ := runCLI(t, ctx, "", cmdExtractArchive, "--input", archive, "--output", out, "--on-conflict", "error", "--password-file", pw); code != 1 {
		t.Fatalf("extract onto itself with --on-conflict error: exit %d %v", code, env)
	}
}

func TestInterruptedCommandExitsWithSignalCode(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(&lifecycle.SignalError{Signal: os.Interrupt})
	code, env, _ := runCLI(t, ctx, "", cmdCompressFile, "--input", filepath.Join(t.TempDir(), "x"))
	if code != 130 {
		t.Fatalf("exit %d, %v", code, env)
	}
}

// TestGlobalOptions: --temp-dir must be an existing directory, -h prints
// the usage, and work directories go in --temp-dir.
func TestGlobalOptions(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(t.TempDir(), "missing"), file} {
		if code, _, stderr := runCLI(t, ctx, "", "--temp-dir", dir, "capabilities"); code != lifecycle.ExitUsage || !strings.Contains(stderr, "--temp-dir") {
			t.Errorf("--temp-dir %s: exit %d, stderr %q; want a usage error naming the flag", dir, code, stderr)
		}
	}

	// A release build has no --faults option; a faultinject build refuses a
	// bad spec.
	if code, _, stderr := runCLI(t, ctx, "", "--faults", "no.such.point=error", "capabilities"); code != lifecycle.ExitUsage || !strings.Contains(stderr, "-faults") {
		t.Errorf("--faults with a bad spec: exit %d, stderr %q; want a usage error naming the option", code, stderr)
	}

	var out, errb bytes.Buffer
	if code := run(ctx, []string{"-h"}, &out, &errb, strings.NewReader("")); code != lifecycle.ExitOK || !strings.Contains(out.String(), "--temp-dir DIR") {
		t.Errorf("-h: exit %d, stdout %q", code, out.String())
	}

	temp, src := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, env, stderr := runCLI(t, ctx, "", "--temp-dir", temp, "create-archive",
		"--input", src, "--output", filepath.Join(t.TempDir(), "a.duesarc"), "--keep-work-dir")
	if code != lifecycle.ExitOK {
		t.Fatalf("create-archive: exit %d, %v, %s", code, env, stderr)
	}
	result, _ := env["result"].(map[string]any)
	if work, _ := result["work_dir"].(string); filepath.Dir(work) != temp {
		t.Fatalf("work directory %q is not in --temp-dir %s (%v)", work, temp, env)
	}
}

// TestSettingsRecord: a run logs the config file, the log and temporary
// directories in effect, and whether an option, the config file or the
// default chose each.
func TestSettingsRecord(t *testing.T) {
	logDir, temp := t.TempDir(), t.TempDir()
	logging.Init(logging.Config{Component: "qwikpak", Dir: logDir, Stderr: io.Discard})
	t.Cleanup(func() { _ = logging.Close() })
	record := func(args ...string) map[string]any {
		t.Helper()
		g, _, err := parseGlobal(append(args, "capabilities"))
		if err != nil {
			t.Fatal(err)
		}
		g.logSettings()
		_ = logging.Sync()
		b, err := os.ReadFile(logging.Current().Path)
		if err != nil {
			t.Fatal(err)
		}
		var rec map[string]any
		for line := range strings.SplitSeq(string(b), "\n") {
			var r map[string]any
			if json.Unmarshal([]byte(line), &r) == nil && r["msg"] == "settings" {
				rec = r
			}
		}
		return rec
	}
	setting := func(rec map[string]any, name string) map[string]any {
		s, _ := rec[name].(map[string]any)
		return s
	}

	rec := record("--temp-dir", temp)
	if s := setting(rec, "temp-dir"); s["value"] != temp || s["source"] != logging.FromFlag {
		t.Errorf("temp-dir %v, want %s from the option", s, temp)
	}
	if s := setting(rec, "log-dir"); s["value"] != logging.Current().Dir || s["source"] != logging.FromDefault {
		t.Errorf("log-dir %v, want the directory in effect, from the default", s)
	}
	if s := setting(rec, "config"); s["value"] != "none" || s["source"] != logging.FromDefault ||
		s["reason"] != "no file at "+filepath.Join("testdata", "no-such-config.json") {
		t.Errorf("config %v, want none: no default file", s)
	}

	config := writeConfig(t, `{"temp-dir": `+jsonPath(t, temp)+`}`)
	rec = record("--config", config)
	if s := setting(rec, "temp-dir"); s["value"] != temp || s["source"] != logging.FromConfig {
		t.Errorf("temp-dir %v, want %s from the config file", s, temp)
	}
	if s := setting(rec, "config"); s["value"] != config || s["source"] != logging.FromFlag {
		t.Errorf("config %v, want %s from the option", s, config)
	}
}
