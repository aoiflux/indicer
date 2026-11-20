package compression_product

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// writeTree creates files (relative path → content) under root; a path ending
// in "/" is an empty directory.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if rel[len(rel)-1] == '/' {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// readTree returns every file (relative slash path → content) and empty
// directory (path ending in "/") under root.
func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if p == root {
				return nil
			}
			entries, err := os.ReadDir(p)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				out[rel+"/"] = ""
			}
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(t *testing.T, got, want map[string]string) {
	t.Helper()
	var missing, wrong, extra []string
	for k, v := range want {
		g, ok := got[k]
		switch {
		case !ok:
			missing = append(missing, k)
		case g != v:
			wrong = append(wrong, k)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(wrong)
	sort.Strings(extra)
	if len(missing)+len(wrong)+len(extra) > 0 {
		t.Fatalf("extracted tree differs: missing %v, wrong content %v, extra %v", missing, wrong, extra)
	}
}

func call(t *testing.T, svc *Service, op string, params map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(params)
	res, rerr := svc.Handle(context.Background(), op, raw)
	if rerr != nil {
		t.Fatalf("%s: %v %v", op, rerr, rerr.Details)
	}
	m, ok := structToMap(res)
	if !ok {
		t.Fatalf("%s: result is not an object", op)
	}
	return m
}

// TestDuesarcFolderRoundTrip: a folder with duplicate files, files of the
// same name in different directories, an empty file and an empty directory
// comes back exactly as it was archived.
func TestDuesarcFolderRoundTrip(t *testing.T) {
	t.Parallel()
	src := filepath.Join(t.TempDir(), "case")
	tree := map[string]string{
		"a.txt":            "same content in two places",
		"sub/a.txt":        "same name, different content",
		"sub/copy.txt":     "same content in two places",
		"sub/deep/b.bin":   "\x00\x01\x02 binary",
		"empty.txt":        "",
		"sub/empty-2.txt":  "",
		"nothing-here/":    "",
		"sub/deep/c.txt":   "third",
		"other/a.txt":      "same content in two places",
		"other/z/last.txt": "last",
	}
	writeTree(t, src, tree)

	out := filepath.Join(t.TempDir(), "case.duesarc")
	svc := New()
	call(t, svc, opCreateArchive, map[string]any{"input_path": src, "output_path": out})

	dst := filepath.Join(t.TempDir(), "restored")
	call(t, svc, opExtractArchive, map[string]any{"input_path": out, "output_path": dst})
	sameTree(t, readTree(t, dst), tree)
}

// TestDuesarcMultiFileRoundTrip: files given one by one, two of them with
// the same base name and two with the same content, all come back.
func TestDuesarcMultiFileRoundTrip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"x/report.txt": "one",
		"y/report.txt": "two",
		"y/dup.txt":    "one",
	})
	out := filepath.Join(t.TempDir(), "bundle.duesarc")
	svc := New()
	call(t, svc, opCreateArchive, map[string]any{
		"input_paths": []string{filepath.Join(root, "x", "report.txt"), filepath.Join(root, "y", "report.txt"), filepath.Join(root, "y", "dup.txt")},
		"output_path": out,
	})
	dst := filepath.Join(t.TempDir(), "restored")
	call(t, svc, opExtractArchive, map[string]any{"input_path": out, "output_path": dst})
	sameTree(t, readTree(t, dst), map[string]string{
		"x/report.txt": "one",
		"y/report.txt": "two",
		"y/dup.txt":    "one",
	})
}
