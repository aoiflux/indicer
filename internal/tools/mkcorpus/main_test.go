package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestFilesLayout: the heavy files sit at a quarter, half, three quarters
// and the end of folder order, and first in the other folder; a tenth of the
// small files copy an earlier one; one seed gives one corpus.
func TestFilesLayout(t *testing.T) {
	s := Spec{Seed: 42, Small: 20, Heavy: 4, SmallMin: kib, SmallMax: 64 * kib, HeavyMin: 100 * kib, HeavyMax: 200 * kib, Copies: 0.1}
	files := s.Files()
	if len(files) != 24 {
		t.Fatalf("%d files, want 24", len(files))
	}
	var heavy []int
	copies := 0
	for i, f := range files {
		if f.Heavy {
			heavy = append(heavy, i)
			if f.Size < s.HeavyMin || f.Size > s.HeavyMax || f.HeavyFirst >= "0004" {
				t.Errorf("heavy file %d: %+v", i, f)
			}
			continue
		}
		if f.Size < s.SmallMin || f.Size > s.SmallMax || f.HeavyFirst < "0004" {
			t.Errorf("small file %d: %+v", i, f)
		}
		if f.CopyOf != "" {
			copies++
			o := files[slices.IndexFunc(files, func(g File) bool { return g.Spread == f.CopyOf })]
			if o.Heavy || o.CopyOf != "" || o.Size != f.Size || o.content != f.content || o.Spread >= f.Spread {
				t.Errorf("file %d copies %+v", i, o)
			}
		}
	}
	if !reflect.DeepEqual(heavy, []int{5, 11, 17, 23}) {
		t.Errorf("heavy files at %v, want [5 11 17 23]", heavy)
	}
	if copies != 2 {
		t.Errorf("%d copies, want 2", copies)
	}
	if again := s.Files(); !reflect.DeepEqual(again, files) {
		t.Error("the same spec gave another corpus")
	}
	s.Seed++
	if other := s.Files(); reflect.DeepEqual(other, files) {
		t.Error("another seed gave the same corpus")
	}
}

// TestWrite: both folders hold the same files, a copy holds its original's
// bytes, the manifest says so, and an existing directory is refused.
func TestWrite(t *testing.T) {
	out := filepath.Join(t.TempDir(), "corpus")
	args := []string{"-out", out, "-small", "10", "-heavy", "2", "-small-min-kib", "1", "-small-max-mib", "1", "-heavy-min-mib", "1", "-heavy-max-mib", "2", "-copies", "0.2"}
	var stderr bytes.Buffer
	if code := run(args, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	raw, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var files []File
	if err := json.Unmarshal(raw, &files); err != nil {
		t.Fatal(err)
	}
	if len(files) != 12 {
		t.Fatalf("manifest lists %d files, want 12", len(files))
	}
	copies := 0
	for _, f := range files {
		p := filepath.Join(out, "spread", f.Spread)
		a, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.Stat(filepath.Join(out, "heavy-first", f.HeavyFirst))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(a, b) || a.Size() != f.Size {
			t.Errorf("%s: %d bytes, heavy-first %s the same file: %v", f.Spread, a.Size(), f.HeavyFirst, os.SameFile(a, b))
		}
		if f.CopyOf != "" {
			copies++
			if !bytes.Equal(readFile(t, p), readFile(t, filepath.Join(out, "spread", f.CopyOf))) {
				t.Errorf("%s does not hold the bytes of %s", f.Spread, f.CopyOf)
			}
		}
	}
	if copies != 2 {
		t.Errorf("%d copies, want 2", copies)
	}
	stderr.Reset()
	if code := run(args, &stderr); code != 1 || !strings.Contains(stderr.String(), "exists") {
		t.Fatalf("a second run: exit %d: %s", code, stderr.String())
	}
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
