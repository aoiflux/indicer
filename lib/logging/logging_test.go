package logging

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"indicer/internal/testutil"
)

// useDirs points Init at dirs only, never at the real cache directory.
func useDirs(t *testing.T, dirs ...string) {
	t.Helper()
	prev := dirCandidates
	dirCandidates = func(explicit string) []string {
		if explicit != "" {
			return append([]string{explicit}, dirs...)
		}
		return dirs
	}
	t.Cleanup(func() {
		_ = Close()
		dirCandidates = prev
	})
}

func readRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("line is not a JSON object: %q: %v", sc.Text(), err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func logFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*"+logExt))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestGetLoggerBeforeInitCreatesNothing(t *testing.T) {
	_ = Close()
	dir := t.TempDir()
	t.Chdir(dir)
	GetLogger().Info("before init", zap.String("k", "v"))
	GetSugar().Infow("before init")
	if err := Sync(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("logging before Init created %v in the working directory", entries)
	}
}

func TestInitWritesNDJSONWithIdentity(t *testing.T) {
	dir := t.TempDir()
	useDirs(t)
	info := Init(Config{Dir: dir, Component: "test", Stderr: io.Discard})
	if info.Dir != dir || info.RunID == "" || !strings.HasPrefix(filepath.Base(info.Path), "test-") {
		t.Fatalf("unexpected Info %+v", info)
	}
	GetLogger().Info("hello", zap.Int("n", 1))
	GetLogger().Warn("second")
	if err := Sync(); err != nil {
		t.Fatal(err)
	}

	recs := readRecords(t, info.Path)
	if len(recs) != 2 {
		t.Fatalf("%d records, want 2", len(recs))
	}
	for _, r := range recs {
		if r["run_id"] != info.RunID || r["component"] != "test" || r["pid"] != float64(os.Getpid()) {
			t.Fatalf("record lacks run_id/pid/component: %v", r)
		}
	}
	if recs[0]["msg"] != "hello" || recs[0]["n"] != float64(1) {
		t.Fatalf("first record %v", recs[0])
	}
}

// TestInitNeverTruncatesEarlierLogs is the LOG-1 regression: the previous log
// was opened with O_TRUNC, so each start destroyed the last crash's log.
func TestInitNeverTruncatesEarlierLogs(t *testing.T) {
	dir := t.TempDir()
	useDirs(t)
	first := Init(Config{Dir: dir, Stderr: io.Discard})
	GetLogger().Info("from the first run")
	second := Init(Config{Dir: dir, Stderr: io.Discard})
	GetLogger().Info("from the second run")
	_ = Sync()

	if first.Path == second.Path {
		t.Fatalf("two runs share %s", first.Path)
	}
	if recs := readRecords(t, first.Path); len(recs) != 1 || recs[0]["msg"] != "from the first run" {
		t.Fatalf("first run's log was altered: %v", recs)
	}
}

func TestInitFallsBackAndNeverFails(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	good := t.TempDir()

	useDirs(t, filepath.Join(notADir, "sub"), good)
	info := Init(Config{Dir: filepath.Join(notADir, "x"), Stderr: io.Discard})
	if info.Dir != good {
		t.Fatalf("logged to %q, want the first usable candidate %q", info.Dir, good)
	}

	useDirs(t, filepath.Join(notADir, "sub"))
	var stderr bytes.Buffer
	info = Init(Config{Stderr: &stderr})
	if info.Dir != "" || info.Path != "" {
		t.Fatalf("Info %+v, want no log file", info)
	}
	if n := strings.Count(stderr.String(), "warning"); n != 1 {
		t.Fatalf("%d warnings on stderr, want 1: %q", n, stderr.String())
	}
	GetLogger().Info("goes nowhere")
}

func TestRotationAndRetention(t *testing.T) {
	dir := t.TempDir()
	useDirs(t)
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(dir, "old-"+string(rune('a'+i))+logExt), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(keep, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	info := Init(Config{Dir: dir, MaxBytes: 2048, Keep: 3, Stderr: io.Discard})
	for i := 0; i < 200; i++ {
		GetLogger().Info("rotation filler entry", zap.Int("i", i))
	}
	_ = Sync()

	files := logFiles(t, dir)
	if len(files) > 3 {
		t.Fatalf("%d log files kept, want at most 3: %v", len(files), files)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("pruning removed a file that is not a log: %v", err)
	}
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() > 2048+512 {
			t.Errorf("%s is %d bytes, rotation limit 2048", filepath.Base(f), st.Size())
		}
	}
	if Current().Path == info.Path {
		t.Fatal("no rotation happened")
	}
}

type failingFile struct{ io.Writer }

func (failingFile) Close() error { return nil }

func TestWriteFailureDegradesOnce(t *testing.T) {
	dir := t.TempDir()
	useDirs(t)
	prev := openFile
	openFile = func(path string) (io.WriteCloser, error) {
		return failingFile{testutil.NewENOSPCWriter(io.Discard, 100)}, nil
	}
	t.Cleanup(func() { openFile = prev })

	var stderr bytes.Buffer
	Init(Config{Dir: dir, Stderr: &stderr})
	for i := 0; i < 50; i++ {
		GetLogger().Info("filling a full disk", zap.Int("i", i))
	}
	if n := strings.Count(stderr.String(), "logging stopped"); n != 1 {
		t.Fatalf("%d degradation warnings, want exactly 1: %q", n, stderr.String())
	}
}

func TestRedaction(t *testing.T) {
	dir := t.TempDir()
	useDirs(t)
	info := Init(Config{Dir: dir, Stderr: io.Discard})
	GetLogger().With(zap.String("db_key", "k3y")).Info("open",
		zap.String("password", "hunter2"),
		zap.String("api_token", "tok123"),
		zap.Int("key_length", 32),
		zap.String("path", "/case/a"),
	)
	_ = Sync()
	raw, err := os.ReadFile(info.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"hunter2", "tok123", "k3y"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("secret %q reached the log: %s", secret, raw)
		}
	}
	rec := readRecords(t, info.Path)[0]
	if rec["key_length"] != float64(32) || rec["path"] != "/case/a" {
		t.Errorf("non-secret fields were changed: %v", rec)
	}
}

func TestRedactArgs(t *testing.T) {
	in := []string{"dues", "store", "-p", "s1", "--password", "s2", "--password=s3", "-ps4",
		"--token", "s5", "--token=s6", "--password-file", "/run/pw", "--preset", "quick", "img.dd"}
	got := strings.Join(RedactArgs(in), " ")
	for _, s := range []string{"s1", "s2", "s3", "s4", "s5", "s6"} {
		if strings.Contains(got, s) {
			t.Errorf("secret %q survived: %s", s, got)
		}
	}
	for _, s := range []string{"/run/pw", "--preset quick", "img.dd"} {
		if !strings.Contains(got, s) {
			t.Errorf("%q was removed: %s", s, got)
		}
	}
	if in[3] != "s1" {
		t.Fatal("RedactArgs modified its input")
	}
}

func TestRedactJSON(t *testing.T) {
	got := string(RedactJSON([]byte(`{"op":"open","password":"p1","nested":{"Token":"t1","items":[{"secret":"x1","size":3}]}}`)))
	for _, s := range []string{"p1", "t1", "x1"} {
		if strings.Contains(got, s) {
			t.Errorf("secret %q survived: %s", s, got)
		}
	}
	if !strings.Contains(got, `"op":"open"`) || !strings.Contains(got, `"size":3`) {
		t.Errorf("non-secret values changed: %s", got)
	}
	if got := string(RedactJSON([]byte(`{"password":"p1"`))); strings.Contains(got, "p1") {
		t.Errorf("unparseable JSON was passed through: %s", got)
	}
}

// FuzzRedactJSON checks that whatever the input, the output is JSON in which
// no sensitive key keeps its value.
func FuzzRedactJSON(f *testing.F) {
	f.Add([]byte(`{"op":"open","password":"p1","nested":{"Token":"t1","items":[{"secret":"x1"}]}}`))
	f.Add([]byte(`{"password":"a","password":{"k":"b"},"db-key":[1,2]}`))
	f.Add([]byte(`[{"api_key":"x"},"password",null]`))
	f.Add([]byte(`{"password"`))
	f.Fuzz(func(t *testing.T, doc []byte) {
		out := RedactJSON(doc)
		var v any
		if err := json.Unmarshal(out, &v); err != nil {
			t.Fatalf("output is not JSON: %q", out)
		}
		var walk func(any)
		walk = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				for k, val := range x {
					if IsSensitiveKey(k) && val != Redacted {
						t.Fatalf("key %q kept value %v in %s", k, val, out)
					}
					walk(val)
				}
			case []any:
				for _, e := range x {
					walk(e)
				}
			}
		}
		walk(v)
	})
}

// FuzzRedactArgs checks that RedactArgs never panics, keeps the shape of the
// command line and redacts every attached secret value.
func FuzzRedactArgs(f *testing.F) {
	f.Add("dues\x00store\x00-p\x00s1\x00--password=s2\x00-ps3")
	f.Add("--token\x00--password\x00x")
	f.Fuzz(func(t *testing.T, line string) {
		in := strings.Split(line, "\x00")
		orig := append([]string(nil), in...)
		out := RedactArgs(in)
		if len(out) != len(in) {
			t.Fatalf("%d args became %d", len(in), len(out))
		}
		for i, a := range out {
			if in[i] != orig[i] {
				t.Fatal("RedactArgs modified its input")
			}
			if name, val, ok := strings.Cut(a, "="); ok && secretFlags[name] && val != Redacted {
				t.Fatalf("attached secret kept: %q", a)
			}
			if strings.HasPrefix(a, "-p") && !strings.HasPrefix(a, "--") && len(a) > 2 && !strings.HasPrefix(a, "-p"+Redacted) && !strings.HasPrefix(a, "-p=") {
				t.Fatalf("short-flag secret kept: %q", a)
			}
		}
	})
}

// TestConcurrentInitAndUse is the LOG-4 regression: the logger was initialised
// lazily without synchronisation. Run with -race.
func TestConcurrentInitAndUse(t *testing.T) {
	dir := t.TempDir()
	useDirs(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			Init(Config{Dir: dir, Stderr: io.Discard})
		}()
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				GetLogger().Info("concurrent", zap.Int("i", i))
			}
		}(i)
	}
	wg.Wait()
}

func TestInitLoggerCompat(t *testing.T) {
	dir := t.TempDir()
	useDirs(t, dir)
	if err := InitLogger(false); err != nil {
		t.Fatal(err)
	}
	if Current().Dir != dir {
		t.Fatalf("InitLogger logged to %q, want %q", Current().Dir, dir)
	}
}
