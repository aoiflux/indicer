package compression_product

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"indicer/internal/testutil"
)

// These tests pass parameters as JSON and compare error codes as strings,
// so they run unchanged against earlier versions of the service.

type entry struct {
	name    string
	content string
	dir     bool
	method  uint16 // zip only; Deflate when zero
}

func makeZip(t *testing.T, path string, entries []entry) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.method != 0 {
			hdr.Method = e.method
		}
		if e.dir {
			hdr.Name = strings.TrimSuffix(e.name, "/") + "/"
			hdr.Method = zip.Store
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if !e.dir {
			if _, err := w.Write([]byte(e.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

type tarEntry struct {
	name     string
	content  string
	typeflag byte
	linkname string
}

func makeTarGz(t *testing.T, path string, entries []tarEntry) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Linkname: e.linkname, Mode: 0o644, Size: int64(len(e.content))}
		if hdr.Typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if hdr.Typeflag != tar.TypeReg {
			hdr.Size = 0
		}
		if hdr.Typeflag == tar.TypeDir {
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// extract runs extract_archive and returns the result map, or the error's
// code and details.
func extract(t *testing.T, params map[string]any) (map[string]any, string, map[string]string) {
	t.Helper()
	raw, _ := json.Marshal(params)
	res, rerr := New().Handle(context.Background(), "extract_archive", raw)
	if rerr != nil {
		return nil, string(rerr.Code), rerr.Details
	}
	m, _ := structToMap(res)
	return m, "", nil
}

// listDir returns every path under dir, directories with a trailing slash.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if info.IsDir() {
			rel += "/"
		}
		out = append(out, rel)
		return nil
	})
	sort.Strings(out)
	return out
}

// TestExtractRefusesUnsafeNames (CMP-6): names that leave the output, name
// an alternate data stream or a reserved device, or that Windows would
// silently change, are refused with a report naming each, and nothing is
// written.
func TestExtractRefusesUnsafeNames(t *testing.T) {
	t.Parallel()
	type bad struct {
		name    string
		windows bool // refused only on Windows
	}
	cases := []bad{
		{"../evil.txt", false},
		{"/abs.txt", false},
		{"dir\\file.txt", false},
		{"a/../../evil.txt", false},
		{"stream.txt:hidden", true},
		{"CON", true},
		{"sub/NUL.txt", true},
		{"trailing.", true},
		{"trailing ", true},
		{"q?.txt", true},
		{"ctl\x01.txt", true},
	}
	for _, c := range cases {
		if c.windows && runtime.GOOS != "windows" {
			continue
		}
		t.Run(fmt.Sprintf("%q", c.name), func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "x.zip")
			makeZip(t, archive, []entry{{name: "good.txt", content: "fine"}, {name: c.name, content: "bad"}})
			out := filepath.Join(dir, "out")
			_, code, details := extract(t, map[string]any{"input_path": archive, "output_path": out})
			if code != "invalid_archive" {
				t.Fatalf("code %q %v, want invalid_archive", code, details)
			}
			if !strings.Contains(details["entry_1"], c.name) {
				t.Fatalf("report does not name the entry: %v", details)
			}
			if got := listDir(t, out); len(got) != 0 {
				t.Fatalf("a refused archive wrote %v", got)
			}
			if _, err := os.Stat(filepath.Join(dir, "evil.txt")); err == nil {
				t.Fatal("an entry escaped the output")
			}
		})
	}
}

// TestExtractRollsBackOnFailure: an archive that fails partway leaves the
// output exactly as it was.
func TestExtractRollsBackOnFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := filepath.Join(dir, "x.zip")
	makeZip(t, archive, []entry{{name: "a.txt", content: "a"}, {name: "sub/b.txt", content: "b"}, {name: "c.txt", content: strings.Repeat("c", 4096)}})
	// Damage the last entry's data, just after its local header's name.
	data, _ := os.ReadFile(archive)
	i := bytes.Index(data, []byte("c.txt"))
	data[i+len("c.txt")+2] ^= 0xff
	if err := os.WriteFile(archive, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "a.txt"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, code, details := extract(t, map[string]any{"input_path": archive, "output_path": out})
	if code == "" {
		t.Fatal("a damaged archive extracted")
	}
	if got := listDir(t, out); strings.Join(got, ",") != "a.txt" {
		t.Fatalf("after a failed extraction the output holds %v (%s %v)", got, code, details)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "a.txt")); string(b) != "original" {
		t.Fatalf("a failed extraction replaced a.txt with %q", b)
	}
}

func TestExtractConflictPolicy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := filepath.Join(dir, "x.zip")
	makeZip(t, archive, []entry{{name: "a.txt", content: "new"}, {name: "b.txt", content: "b"}})
	prepare := func(name string) string {
		out := filepath.Join(dir, name)
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, "a.txt"), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		return out
	}
	read := func(out, name string) string {
		b, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			return "<missing>"
		}
		return string(b)
	}

	out := prepare("overwrite")
	if _, code, d := extract(t, map[string]any{"input_path": archive, "output_path": out}); code != "" {
		t.Fatalf("overwrite: %s %v", code, d)
	}
	if read(out, "a.txt") != "new" || read(out, "b.txt") != "b" {
		t.Fatalf("overwrite: a=%q b=%q", read(out, "a.txt"), read(out, "b.txt"))
	}

	out = prepare("skip")
	if _, code, d := extract(t, map[string]any{"input_path": archive, "output_path": out, "on_conflict": "skip"}); code != "" {
		t.Fatalf("skip: %s %v", code, d)
	}
	if read(out, "a.txt") != "old" || read(out, "b.txt") != "b" {
		t.Fatalf("skip: a=%q b=%q", read(out, "a.txt"), read(out, "b.txt"))
	}

	out = prepare("error")
	if _, code, d := extract(t, map[string]any{"input_path": archive, "output_path": out, "on_conflict": "error"}); code != "conflict" || !strings.Contains(d["entry_1"], "a.txt") {
		t.Fatalf("error: %q %v, want conflict naming a.txt", code, d)
	}
	if read(out, "a.txt") != "old" || read(out, "b.txt") != "<missing>" {
		t.Fatalf("error: a=%q b=%q", read(out, "a.txt"), read(out, "b.txt"))
	}

	if _, code, _ := extract(t, map[string]any{"input_path": archive, "output_path": out, "on_conflict": "rename"}); code != "invalid_request" {
		t.Fatalf("unknown policy: %q", code)
	}
}

func TestExtractLimits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := filepath.Join(dir, "x.zip")
	makeZip(t, archive, []entry{{name: "a.txt", content: strings.Repeat("a", 1000)}, {name: "deep/er/b.txt", content: "b"}, {name: "c.txt", content: "c"}})
	for _, c := range []struct {
		limits map[string]any
		limit  string
	}{
		{map[string]any{"max_entries": 2}, "max_entries"},
		{map[string]any{"max_total_bytes": 500}, "max_total_bytes"},
		{map[string]any{"max_entry_bytes": 999}, "max_entry_bytes"},
		{map[string]any{"max_path_bytes": 10}, "max_path_bytes"},
	} {
		out := filepath.Join(dir, c.limit)
		_, code, details := extract(t, map[string]any{"input_path": archive, "output_path": out, "limits": c.limits})
		if code != "limit_exceeded" || details["limit"] != c.limit {
			t.Errorf("%v: %q %v, want limit_exceeded on %s", c.limits, code, details, c.limit)
		}
		if got := listDir(t, out); len(got) != 0 {
			t.Errorf("%v: wrote %v", c.limits, got)
		}
	}
}

func TestExtractCompressionRatio(t *testing.T) {
	defer func(g int64) { ratioGrace = g }(ratioGrace)
	ratioGrace = 64 << 10
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "x.zip")
	makeZip(t, zipPath, []entry{{name: "zeros.bin", content: string(make([]byte, 1<<20))}})
	tgzPath := filepath.Join(dir, "x.tar.gz")
	makeTarGz(t, tgzPath, []tarEntry{{name: "zeros.bin", content: string(make([]byte, 1<<20))}})
	for _, archive := range []string{zipPath, tgzPath} {
		out := filepath.Join(dir, filepath.Base(archive)+".out")
		_, code, details := extract(t, map[string]any{"input_path": archive, "output_path": out, "limits": map[string]any{"max_ratio": 100}})
		if code != "limit_exceeded" || details["limit"] != "max_ratio" {
			t.Errorf("%s: %q %v, want limit_exceeded on max_ratio", filepath.Base(archive), code, details)
		}
		if _, code, details := extract(t, map[string]any{"input_path": archive, "output_path": out + "2"}); code != "" {
			t.Errorf("%s with the default ratio: %q %v", filepath.Base(archive), code, details)
		}
	}
}

// TestExtractDuplicateEntries: a second entry of the same name (or, where
// names fold case, the same name in another case) is refused, not written
// over the first.
func TestExtractDuplicateEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	names := []string{"a.txt", "a.txt"}
	if runtime.GOOS == "windows" {
		names = []string{"a.txt", "A.TXT"}
	}
	archive := filepath.Join(dir, "x.zip")
	makeZip(t, archive, []entry{{name: names[0], content: "first"}, {name: names[1], content: "second"}})
	out := filepath.Join(dir, "out")
	_, code, details := extract(t, map[string]any{"input_path": archive, "output_path": out})
	if code != "invalid_archive" || !strings.Contains(details["entry_1"], names[1]) {
		t.Fatalf("%q %v, want invalid_archive naming %s", code, details, names[1])
	}
}

// TestExtractTarSpecialEntries: links and devices are not created; they are
// counted and named in the result.
func TestExtractTarSpecialEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := filepath.Join(dir, "x.tar.gz")
	makeTarGz(t, archive, []tarEntry{
		{name: "./", typeflag: tar.TypeDir},
		{name: "./dir/", typeflag: tar.TypeDir},
		{name: "./dir/a.txt", content: "a"},
		{name: "./link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		{name: "./hard", typeflag: tar.TypeLink, linkname: "dir/a.txt"},
		{name: "./dev", typeflag: tar.TypeChar},
	})
	out := filepath.Join(dir, "out")
	res, code, details := extract(t, map[string]any{"input_path": archive, "output_path": out})
	if code != "" {
		t.Fatalf("%q %v", code, details)
	}
	if got := strings.Join(listDir(t, out), ","); got != "dir/,dir/a.txt" {
		t.Fatalf("output holds %s", got)
	}
	if n, _ := res["skipped_entries"].(float64); n != 3 {
		t.Fatalf("skipped_entries %v, want 3 (%v)", res["skipped_entries"], res)
	}
}

// TestExtractDiskFullRefusedUpFront runs against a small tmpfs (scripts/wsl/mounts.sh).
func TestExtractDiskFullRefusedUpFront(t *testing.T) {
	t.Parallel()
	small := testutil.ENOSPCDir.Require(t)
	dir := t.TempDir()
	archive := filepath.Join(dir, "big.zip")
	makeZip(t, archive, []entry{{name: "big.bin", content: string(make([]byte, 64<<20))}})
	out, err := os.MkdirTemp(small, "extract-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(out)
	_, code, details := extract(t, map[string]any{"input_path": archive, "output_path": out})
	if code != "limit_exceeded" || details["limit"] != "free_space" {
		t.Fatalf("%q %v, want limit_exceeded on free_space", code, details)
	}
	if got := listDir(t, out); len(got) != 0 {
		t.Fatalf("wrote %v", got)
	}
}

// TestExtractDoesNotFollowLinksOut: a symbolic link already in the output
// that points elsewhere does not carry an entry outside the output.
func TestExtractDoesNotFollowLinksOut(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(out, "link")); err != nil {
		t.Skipf("cannot create a symbolic link here: %v", err)
	}
	archive := filepath.Join(dir, "x.zip")
	makeZip(t, archive, []entry{{name: "link/planted.txt", content: "x"}})
	_, code, details := extract(t, map[string]any{"input_path": archive, "output_path": out})
	if code == "" {
		t.Fatalf("extracted through a link out of the output (%v)", details)
	}
	if _, err := os.Stat(filepath.Join(outside, "planted.txt")); err == nil {
		t.Fatal("an entry was written outside the output")
	}
}
