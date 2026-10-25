package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
)

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
	t.Setenv(envPassword, "from-env")
	cases := []struct {
		file   string
		stdin  bool
		value  string
		want   string
		warned bool
	}{
		{file: file, stdin: true, value: "x", want: "from-file"},
		{stdin: true, value: "x", want: "from-stdin"},
		{value: "x", want: "from-env"},
	}
	for _, c := range cases {
		var errb bytes.Buffer
		r := &runner{stderr: &errb, stdin: strings.NewReader("from-stdin\nmore")}
		got, err := r.password(passwordFlags{file: &c.file, stdin: &c.stdin, value: &c.value})
		if err != nil || got != c.want {
			t.Errorf("%+v: %q, %v", c, got, err)
		}
	}
	t.Setenv(envPassword, "")
	var errb bytes.Buffer
	r := &runner{stderr: &errb, stdin: strings.NewReader("")}
	empty, no, value := "", false, "from-flag"
	if got, _ := r.password(passwordFlags{file: &empty, stdin: &no, value: &value}); got != "from-flag" || !strings.Contains(errb.String(), "visible") {
		t.Fatalf("--password: %q, warning %q", got, errb.String())
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
	t.Setenv(envPassword, "")
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
	t.Setenv(envPassword, "wrong")
	if code, _, _ := runCLI(t, ctx, "", cmdExtractArchive, "--input", archive, "--output", filepath.Join(dir, "x")); code != 2 {
		t.Fatalf("extract with a wrong password: exit %d", code)
	}
	t.Setenv(envPassword, "s3cret-pass")
	out := filepath.Join(dir, "out")
	if code, env, _ := runCLI(t, ctx, "", cmdExtractArchive, "--input", archive, "--output", out); code != 0 {
		t.Fatalf("extract: exit %d %v", code, env)
	}
	for name, want := range map[string]string{"a.txt": "alpha", "sub/b.txt": "beta", "sub/c.txt": "alpha"} {
		if got, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(name))); err != nil || string(got) != want {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	if code, env, _ := runCLI(t, ctx, "", cmdExtractArchive, "--input", archive, "--output", out, "--on-conflict", "error"); code != 1 {
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
