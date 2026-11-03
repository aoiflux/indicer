package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/testutil/fsfixture"
	"indicer/internal/testutil/leakcheck"
)

func TestMain(m *testing.M) {
	code := leakcheck.Run(m)
	fsfixture.Cleanup()
	os.Exit(code)
}

func fixtureManifest(t *testing.T, name string) string {
	t.Helper()
	for _, m := range fsfixture.All(t) {
		if m.Name != name {
			continue
		}
		out := filepath.Join(t.TempDir(), "m.jsonl")
		var stderr bytes.Buffer
		if code := run([]string{"-o", out, fsfixture.Open(t, m)}, &bytes.Buffer{}, &stderr); code != 0 {
			t.Fatalf("run exit %d: %s", code, stderr.String())
		}
		return out
	}
	t.Fatalf("no fixture %s", name)
	return ""
}

func TestManifestDigestsAgree(t *testing.T) {
	out := fixtureManifest(t, "fat/fat16")
	idx, err := readManifest(out)
	if err != nil {
		t.Fatal(err)
	}
	if idx.summary.Files == 0 || idx.summary.Panics != 0 {
		t.Fatalf("summary %+v", idx.summary)
	}
	for k, fs := range idx.files {
		for _, f := range fs {
			if f.Kind == "file" && !k.Deleted && f.SHA256Runs != "" && f.SHA256Open != "" && f.SHA256Runs != f.SHA256Open {
				t.Errorf("%s: runs and Open digests differ", k.Path)
			}
		}
	}
}

func TestDiffSameAndChanged(t *testing.T) {
	a := fixtureManifest(t, "fat/fat12")
	var stdout, stderr bytes.Buffer
	if code := runDiff([]string{a, a}, &stdout, &stderr); code != 0 {
		t.Fatalf("self diff exit %d: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "_changed") || strings.Contains(stdout.String(), "_added") {
		t.Fatalf("self diff reported differences:\n%s", stdout.String())
	}

	// Change one file's size and digest; the diff must report both and an
	// allowlist that does not cover them must fail.
	raw, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	for i, l := range lines {
		if strings.Contains(l, `"path":"/hello.txt"`) {
			l = strings.Replace(l, `"size":11`, `"size":12`, 1)
			l = strings.Replace(l, `"sha256_runs":"`, `"sha256_runs":"00`, 1)
			lines[i] = l
		}
	}
	b := filepath.Join(t.TempDir(), "b.jsonl")
	if err := os.WriteFile(b, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	allow := filepath.Join(t.TempDir(), "allow.json")
	if err := os.WriteFile(allow, []byte(`{"fat12.img": ["size_changed"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := runDiff([]string{"-allow", allow, a, b}, &stdout, &stderr); code != 1 {
		t.Fatalf("diff exit %d, want 1:\n%s", code, stdout.String())
	}
	for _, want := range []string{"size_changed", "content_changed", "UNEXPECTED"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("diff output lacks %q:\n%s", want, stdout.String())
		}
	}
}

func TestDiffRejectsUnknownCategory(t *testing.T) {
	a := fixtureManifest(t, "fat/fat12")
	allow := filepath.Join(t.TempDir(), "allow.json")
	if err := os.WriteFile(allow, []byte(`{"fat12.img": ["sise_changed"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runDiff([]string{"-allow", allow, a, a}, &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
		t.Fatalf("exit %d, want 2 for a misspelt category", code)
	}
}

func TestRunRejectsBadFlags(t *testing.T) {
	for _, args := range [][]string{{}, {"-hash", "md5", "x"}, {"-max-hash", "-1", "x"}} {
		if code := run(args, &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
			t.Errorf("run(%q) exit %d, want 2", args, code)
		}
	}
	if code := run([]string{filepath.Join(t.TempDir(), "missing.img")}, &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Errorf("missing image: exit %d, want 1", code)
	}
}
