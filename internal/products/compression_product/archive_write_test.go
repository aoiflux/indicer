package compression_product

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"indicer/internal/duesarc"
	"indicer/internal/testutil"
)

func create(t *testing.T, params map[string]any) (map[string]any, string, map[string]string) {
	t.Helper()
	res, rerr := handle(New(), opCreateArchive, params)
	if rerr != nil {
		return nil, string(rerr.Code), rerr.Details
	}
	m, _ := structToMap(res)
	return m, "", nil
}

// TestArchiveWriteFailureLeavesNoOutput (CMP-2): a write that fails at any
// point, the closing writes included (zip central directory, gzip and xz
// trailers), is reported, and leaves an earlier archive of the same name as
// it was.
func TestArchiveWriteFailureLeavesNoOutput(t *testing.T) {
	src := t.TempDir()
	testutil.WriteRandomFile(t, src, "data.bin", 1, 200_000)
	if err := os.WriteFile(filepath.Join(src, "note.txt"), []byte(strings.Repeat("note ", 1000)), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"zip", "tar", "tar.gz", "tar.xz", "duesarc"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "archive"+archiveExtensionForFormat(format))
			if _, code, d := create(t, map[string]any{"input_path": src, "output_path": out, "format": format}); code != "" {
				t.Fatalf("create: %s %v", code, d)
			}
			good, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			defer func(prev func(*os.File) io.Writer) { duesarc.Sink = prev }(duesarc.Sink)
			// A .duesarc archive's size varies by a few bytes between runs
			// (its database holds times and IDs), so its last failure point
			// stays well inside the central directory.
			offsets := []int64{0, 100, int64(len(good)) / 2, int64(len(good)) - 200}
			if format != "duesarc" {
				offsets = append(offsets, int64(len(good))-1)
			}
			for _, after := range offsets {
				duesarc.Sink = func(f *os.File) io.Writer { return testutil.NewENOSPCWriter(f, after) }
				if _, code, _ := create(t, map[string]any{"input_path": src, "output_path": out, "format": format}); code == "" {
					t.Errorf("a write failing after %d of %d bytes reported success", after, len(good))
				}
				if now, err := os.ReadFile(out); err != nil || string(now) != string(good) {
					t.Errorf("a write failing after %d bytes changed the earlier archive (%d bytes, %v)", after, len(now), err)
				}
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 1 {
				t.Errorf("failed writes left %v", entries)
			}
		})
	}
}

func readSidecar(t *testing.T, path string) duesarc.Sidecar {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var sc duesarc.Sidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		t.Fatal(err)
	}
	return sc
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestSplitArchiveSidecar (CMP-3): split parts are described by a sidecar
// that readers check, so a damaged, missing or extra part is refused.
func TestSplitArchiveSidecar(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"duesarc", "zip", "tar.gz"} {
		t.Run(format, func(t *testing.T) {
			src := t.TempDir()
			testutil.WriteRandomFile(t, src, "big.bin", 7, 2_500_000)
			dir := t.TempDir()
			out := filepath.Join(dir, "set"+archiveExtensionForFormat(format))
			res, code, d := create(t, map[string]any{"input_path": src, "output_path": out, "format": format, "split_size_mb": 1})
			if code != "" {
				t.Fatalf("create: %s %v", code, d)
			}
			parts, _ := res["output_files"].([]any)
			if len(parts) != 3 {
				t.Fatalf("parts %v", res["output_files"])
			}
			sc := readSidecar(t, out+duesarc.SidecarExt)
			if len(sc.Parts) != 3 {
				t.Fatalf("sidecar %+v", sc)
			}
			for i, p := range sc.Parts {
				if p.Name != filepath.Base(parts[i].(string)) || p.SHA256 != fileSHA256(t, parts[i].(string)) {
					t.Fatalf("sidecar part %d %+v does not describe %s", i, p, parts[i])
				}
			}
			if _, err := os.Stat(out); err == nil {
				t.Fatal("the unsplit archive was left beside its parts")
			}

			first := parts[0].(string)
			if _, code, d := extract(t, map[string]any{"input_path": first, "output_path": filepath.Join(dir, "ok")}); code != "" {
				t.Fatalf("extract: %s %v", code, d)
			}
			if got, want := fileSHA256(t, filepath.Join(dir, "ok", "big.bin")), fileSHA256(t, filepath.Join(src, "big.bin")); got != want {
				t.Fatal("split archive extracted wrong content")
			}

			// A damaged part.
			data, _ := os.ReadFile(parts[1].(string))
			data[len(data)/2] ^= 0xff
			if err := os.WriteFile(parts[1].(string), data, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, code, d := extract(t, map[string]any{"input_path": first, "output_path": filepath.Join(dir, "bad")}); code != "invalid_archive" {
				t.Fatalf("damaged part: %q %v, want invalid_archive", code, d)
			}
			// A missing part.
			if err := os.Remove(parts[2].(string)); err != nil {
				t.Fatal(err)
			}
			if _, code, d := extract(t, map[string]any{"input_path": first, "output_path": filepath.Join(dir, "missing")}); code != "invalid_archive" {
				t.Fatalf("missing part: %q %v, want invalid_archive", code, d)
			}
		})
	}
}

// TestSplitReplacesEarlierOutput: writing an archive replaces what an
// earlier one of the same name left, parts included.
func TestSplitReplacesEarlierOutput(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	testutil.WriteRandomFile(t, src, "big.bin", 3, 3_500_000)
	dir := t.TempDir()
	out := filepath.Join(dir, "set.zip")
	if _, code, d := create(t, map[string]any{"input_path": src, "output_path": out, "format": "zip", "split_size_mb": 1}); code != "" {
		t.Fatalf("create: %s %v", code, d)
	}
	if _, err := os.Stat(out + ".004"); err != nil {
		t.Fatalf("four parts expected: %v", err)
	}
	if err := os.Remove(filepath.Join(src, "big.bin")); err != nil {
		t.Fatal(err)
	}
	testutil.WriteRandomFile(t, src, "small.bin", 4, 1_500_000)
	if _, code, d := create(t, map[string]any{"input_path": src, "output_path": out, "format": "zip", "split_size_mb": 1}); code != "" {
		t.Fatalf("second create: %s %v", code, d)
	}
	for _, stale := range []string{".003", ".004"} {
		if _, err := os.Stat(out + stale); err == nil {
			t.Errorf("stale part %s left behind", stale)
		}
	}
	res, code, d := extract(t, map[string]any{"input_path": out + ".001", "output_path": filepath.Join(dir, "x")})
	if code != "" || res["extracted_files"] != float64(1) {
		t.Fatalf("extract: %s %v %v", code, d, res)
	}
	// Now unsplit: the parts and sidecar go.
	if _, code, d := create(t, map[string]any{"input_path": src, "output_path": out, "format": "zip"}); code != "" {
		t.Fatalf("third create: %s %v", code, d)
	}
	if left, _ := filepath.Glob(out + ".*"); len(left) != 0 {
		t.Errorf("unsplit archive left %v", left)
	}
}

// TestNumberedFileIsNotASplitPart (CMP-3): image.001 is a file, not the
// first part of an archive, and its neighbours are not concatenated to it.
func TestNumberedFileIsNotASplitPart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	makeZip(t, filepath.Join(dir, "image.001"), []entry{{name: "one.txt", content: "one"}})
	if err := os.WriteFile(filepath.Join(dir, "image.002"), []byte("unrelated bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, code, d := extract(t, map[string]any{"input_path": filepath.Join(dir, "image.001"), "output_path": filepath.Join(dir, "out")})
	if code != "" || res["extracted_files"] != float64(1) {
		t.Fatalf("%q %v %v", code, d, res)
	}
}

// TestSplitReadingLeavesNoTemporaryFiles: reading a split archive used to
// assemble it in a temporary directory that was never removed.
func TestSplitReadingLeavesNoTemporaryFiles(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	svc := NewWithOptions(Options{TempDir: tmp})
	src := t.TempDir()
	testutil.WriteRandomFile(t, src, "big.bin", 9, 1_500_000)
	dir := t.TempDir()
	out := filepath.Join(dir, "set.duesarc")
	if _, code, d := create(t, map[string]any{"input_path": src, "output_path": out, "split_size_mb": 1}); code != "" {
		t.Fatalf("create: %s %v", code, d)
	}
	for _, op := range []string{opListArchive, opVerifyArchive, opExtractArchive} {
		params := map[string]any{"input_path": out + ".001"}
		if op == opExtractArchive {
			params["output_path"] = filepath.Join(dir, "x")
		}
		if _, rerr := handle(svc, op, params); rerr != nil {
			t.Fatalf("%s: %v %v", op, rerr, rerr.Details)
		}
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

// TestWorkDirsGoInTempDir: work directories go in Options.TempDir
// (--temp-dir), and the first one sweeps the stale ones left there.
func TestWorkDirsGoInTempDir(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	stale := filepath.Join(tmp, "duesarc-work-stale")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleWorkAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	testutil.WriteRandomFile(t, src, "a.bin", 3, 64<<10)
	res, rerr := handle(NewWithOptions(Options{TempDir: tmp}), opCreateArchive, map[string]any{
		"input_path": src, "output_path": filepath.Join(t.TempDir(), "a.duesarc"), "keep_work_dir": true,
	})
	if rerr != nil {
		t.Fatalf("create: %v %v", rerr, rerr.Details)
	}
	m, _ := structToMap(res)
	if work, _ := m["work_dir"].(string); filepath.Dir(work) != tmp {
		t.Fatalf("work directory %q is not in Options.TempDir %s", work, tmp)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale work directory not swept: %v", err)
	}
}
