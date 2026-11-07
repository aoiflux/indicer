package compression_product

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"

	"indicer/internal/duesarc"
	"indicer/internal/testutil"
	"indicer/lib/dbio"
	"indicer/lib/dbopen"
	"indicer/lib/ingest"
	"indicer/lib/structs"
	"indicer/lib/util"
)

// rewriteZip rewrites the zip at p, passing each entry's content through fn;
// fn returning nil drops the entry.
func rewriteZip(t *testing.T, p string, fn func(name string, data []byte) []byte) {
	t.Helper()
	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if data = fn(f.Name, data); data == nil {
			continue
		}
		hdr := f.FileHeader
		w, err := zw.CreateHeader(&hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	zr.Close()
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func editManifest(t *testing.T, archive string, fn func(m *archiveManifest)) {
	t.Helper()
	rewriteZip(t, archive, func(name string, data []byte) []byte {
		if name != manifestFileName {
			return data
		}
		var m archiveManifest
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		fn(&m)
		out, _ := json.Marshal(m)
		return out
	})
}

func TestCompressFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "report.bin")
	testutil.WriteRandomFile(t, dir, "report.bin", 11, 700_000)
	want := fileSHA256(t, in)
	svc := New()
	res, rerr := handle(svc, opCompressFile, map[string]any{"input_path": in})
	if rerr != nil {
		t.Fatalf("compress_file: %v %v", rerr, rerr.Details)
	}
	archive := res.(fileRes).OutputPath
	if archive != in+archiveExt {
		t.Fatalf("archive written to %s, want beside the input", archive)
	}
	out := filepath.Join(dir, "back.bin")
	dres, rerr := handle(svc, opDecompressFile, map[string]any{"input_path": archive, "output_path": out})
	if rerr != nil {
		t.Fatalf("decompress_file: %v %v", rerr, rerr.Details)
	}
	if fileSHA256(t, out) != want || dres.(fileRes).Format != "duesarc" {
		t.Fatalf("round trip gave different bytes (%+v)", dres)
	}
	// Without output_path, the archive's extension comes off.
	if err := os.Remove(in); err != nil {
		t.Fatal(err)
	}
	if _, rerr := handle(svc, opDecompressFile, map[string]any{"input_path": archive}); rerr != nil {
		t.Fatalf("decompress_file to the default path: %v", rerr)
	}
	if fileSHA256(t, in) != want {
		t.Fatal("default output differs")
	}

	// An archive of a folder is not one file.
	folder := filepath.Join(dir, "folder")
	writeTree(t, folder, map[string]string{"a.txt": "a", "b.txt": "b"})
	multi := filepath.Join(dir, "folder.duesarc")
	call(t, svc, opCreateArchive, map[string]any{"input_path": folder, "output_path": multi})
	if _, rerr := handle(svc, opDecompressFile, map[string]any{"input_path": multi, "output_path": filepath.Join(dir, "x")}); rerr == nil || rerr.Code != "invalid_request" {
		t.Fatalf("a folder archive: %v", rerr)
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); err == nil {
		t.Fatal("a refused decompression wrote its output")
	}
}

// makeV1Archive builds a version 1 archive of the files under src the way
// earlier versions did: one evidence per stored file (an alias for repeated
// content, since W6) and a manifest naming only the input folder. With
// namesOnly, the records carry only base names, as before W6.
func makeV1Archive(t *testing.T, src, archive string, namesOnly bool) {
	t.Helper()
	work := t.TempDir()
	dbPath := filepath.Join(work, manifestDbRelativePath)
	ctx := context.Background()
	err := func() error {
		h, err := openArchiveDB(ctx, dbPath, "", 256, dbopen.ReadWrite)
		if err != nil {
			return err
		}
		defer h.Close()
		if err := util.EnsureBlobPath(h.Root); err != nil {
			return err
		}
		return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			res, err := ingest.Run(ctx, h.DB, ingest.Request{Path: p, Decode: false, NoIndex: true, Quiet: true})
			if err != nil {
				return err
			}
			if namesOnly {
				return dbio.UpdateEvidence(h.DB, res.EvidenceID, func(_ *badger.Txn, e *structs.EvidenceFile) error {
					e.Name = filepath.Base(p)
					e.Source = nil
					return nil
				})
			}
			return nil
		})
	}()
	if err != nil {
		t.Fatal(err)
	}
	m := archiveManifest{Version: manifestVersionV1, Pipeline: manifestPipelineStore, DbPath: manifestDbRelativePath, ChunkSizeKB: 256, InputPath: src, InputIsDir: true}
	data, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(work, manifestFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := duesarc.WriteOutput(ctx, archive, 0, func(w io.Writer) error { return writeDuesarcZip(ctx, w, work, nil) }); err != nil {
		t.Fatal(err)
	}
}

// TestExtractV1Archives: version 1 archives still extract. Those written
// since W6 record each file's source path and alias, so the tree and its
// duplicates come back; older ones only names, so files of the same name get
// distinct names instead of overwriting each other.
func TestExtractV1Archives(t *testing.T) {
	src := filepath.Join(t.TempDir(), "case")
	tree := map[string]string{"a.txt": "same", "sub/a.txt": "different", "sub/dup.txt": "same"}
	writeTree(t, src, tree)

	withPaths := filepath.Join(t.TempDir(), "v1.duesarc")
	makeV1Archive(t, src, withPaths, false)
	out := filepath.Join(t.TempDir(), "out")
	call(t, New(), opExtractArchive, map[string]any{"input_path": withPaths, "output_path": out})
	sameTree(t, readTree(t, out), tree)

	// Before W6 every file was its own evidence; this tree has no repeated
	// content, so none becomes an alias.
	src = filepath.Join(t.TempDir(), "old")
	writeTree(t, src, map[string]string{"a.txt": "one", "sub/a.txt": "two"})
	namesOnly := filepath.Join(t.TempDir(), "old.duesarc")
	makeV1Archive(t, src, namesOnly, true)
	out = filepath.Join(t.TempDir(), "out")
	call(t, New(), opExtractArchive, map[string]any{"input_path": namesOnly, "output_path": out})
	got := readTree(t, out)
	if len(got) != 2 || got["a.txt"] == "" {
		t.Fatalf("names-only v1 archive gave %v", got)
	}
	var renamed string
	for k := range got {
		if k != "a.txt" {
			renamed = k
		}
	}
	if !strings.HasPrefix(renamed, "a~") || !strings.HasSuffix(renamed, ".txt") || got[renamed] == got["a.txt"] {
		t.Fatalf("second a.txt restored as %q (%v)", renamed, got)
	}
}

func TestVerifyArchiveChecksContent(t *testing.T) {
	src := filepath.Join(t.TempDir(), "case")
	writeTree(t, src, map[string]string{"one.txt": strings.Repeat("one ", 5000), "two/two.txt": strings.Repeat("two ", 5000)})
	archive := filepath.Join(t.TempDir(), "case.duesarc")
	call(t, New(), opCreateArchive, map[string]any{"input_path": src, "output_path": archive, "password": "correct horse", "chunk_size_kb": 4})

	if _, rerr := handle(New(), opVerifyArchive, map[string]any{"input_path": archive}); rerr == nil || rerr.Details["reason"] != "password_required" {
		t.Fatalf("no password: %v", rerr)
	}
	if _, rerr := handle(New(), opVerifyArchive, map[string]any{"input_path": archive, "password": "wrong"}); rerr == nil || rerr.Details["reason"] != "wrong_password" {
		t.Fatalf("wrong password: %v", rerr)
	}
	res := call(t, New(), opVerifyArchive, map[string]any{"input_path": archive, "password": "correct horse"})
	if res["valid"] != true || res["verified_evidence"] != float64(2) || res["file_count"] != float64(2) {
		t.Fatalf("verify: %v", res)
	}

	// Damage one stored chunk; the zip itself stays consistent.
	damaged := false
	rewriteZip(t, archive, func(name string, data []byte) []byte {
		if !damaged && strings.HasPrefix(name, "db/blob/") && !strings.HasSuffix(name, "/") && len(data) > 40 {
			data = append([]byte(nil), data...)
			data[len(data)-5] ^= 0xff
			damaged = true
		}
		return data
	})
	if !damaged {
		t.Fatal("no chunk blob to damage")
	}
	_, rerr := handle(New(), opVerifyArchive, map[string]any{"input_path": archive, "password": "correct horse"})
	if rerr == nil || rerr.Code != "invalid_archive" || !strings.Contains(rerr.Details["entry_1"], ".txt") {
		t.Fatalf("damaged content: %v %v", rerr, rerr.Details)
	}
	if _, rerr := handle(New(), opExtractArchive, map[string]any{"input_path": archive, "password": "correct horse", "output_path": filepath.Join(t.TempDir(), "x")}); rerr == nil || rerr.Code != "invalid_archive" {
		t.Fatalf("extracting damaged content: %v", rerr)
	}
}

// TestDuesarcDeduplicationBomb: content stored once but listed under many
// paths counts once per path against the limits (CMP-1).
func TestDuesarcDeduplicationBomb(t *testing.T) {
	src := t.TempDir()
	testutil.WriteRandomFile(t, src, "one.bin", 5, 1<<20)
	archive := filepath.Join(t.TempDir(), "bomb.duesarc")
	call(t, New(), opCreateArchive, map[string]any{"input_path": filepath.Join(src, "one.bin"), "output_path": archive})
	editManifest(t, archive, func(m *archiveManifest) {
		e := m.Entries[0]
		m.Entries = nil
		for i := 0; i < 100; i++ {
			e.Path = "copy-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + "-" + time.Duration(i).String() + ".bin"
			m.Entries = append(m.Entries, e)
		}
	})
	out := filepath.Join(t.TempDir(), "out")
	_, rerr := handle(New(), opExtractArchive, map[string]any{"input_path": archive, "output_path": out, "limits": map[string]any{"max_total_bytes": 50 << 20}})
	if rerr == nil || rerr.Code != "limit_exceeded" || rerr.Details["limit"] != "max_total_bytes" {
		t.Fatalf("100 copies of 1 MiB with a 50 MiB limit: %v", rerr)
	}
	if _, err := os.Stat(out); err == nil {
		if entries, _ := os.ReadDir(out); len(entries) != 0 {
			t.Fatalf("wrote %v", entries)
		}
	}
	res := call(t, New(), opExtractArchive, map[string]any{"input_path": archive, "output_path": filepath.Join(t.TempDir(), "ok")})
	if res["extracted_files"] != float64(100) {
		t.Fatalf("with the default limit: %v", res)
	}
}

func TestDuesarcManifestIsChecked(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{"a.txt": "a"})
	good := filepath.Join(t.TempDir(), "good.duesarc")
	call(t, New(), opCreateArchive, map[string]any{"input_path": src, "output_path": good})
	data, _ := os.ReadFile(good)
	for name, edit := range map[string]func(m *archiveManifest){
		"db outside":      func(m *archiveManifest) { m.DbPath = "../outside" },
		"unknown version": func(m *archiveManifest) { m.Version = "v9" },
		"missing content": func(m *archiveManifest) {
			m.Entries[0].Evidence = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
		},
		"wrong size":         func(m *archiveManifest) { m.Entries[0].Size++ },
		"entry outside":      func(m *archiveManifest) { m.Entries[0].Path = "../a.txt" },
		"unknown entry type": func(m *archiveManifest) { m.Entries[0].Type = "link" },
	} {
		t.Run(name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "bad.duesarc")
			if err := os.WriteFile(archive, data, 0o644); err != nil {
				t.Fatal(err)
			}
			editManifest(t, archive, edit)
			out := filepath.Join(t.TempDir(), "out")
			_, rerr := handle(New(), opExtractArchive, map[string]any{"input_path": archive, "output_path": out, "format": "duesarc"})
			if rerr == nil || rerr.Code != "invalid_archive" {
				t.Fatalf("%v %v, want invalid_archive", rerr, rerr.Details)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(out), "outside")); err == nil {
				t.Fatal("wrote outside the output")
			}
		})
	}
}

func TestDuesarcKeepsTimesAndSkipsLinks(t *testing.T) {
	src := filepath.Join(t.TempDir(), "case")
	writeTree(t, src, map[string]string{"a.txt": "a", "d/b.txt": "b"})
	when := time.Date(2020, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(src, "d", "b.txt"), when, when); err != nil {
		t.Fatal(err)
	}
	linked := os.Symlink(filepath.Join(src, "a.txt"), filepath.Join(src, "link.txt")) == nil
	archive := filepath.Join(t.TempDir(), "case.duesarc")
	res := call(t, New(), opCreateArchive, map[string]any{"input_path": src, "output_path": archive})
	if linked && (res["skipped_inputs"] != float64(1) || !strings.Contains(strings.Join(toStrings(res["skipped"]), ","), "link.txt")) {
		t.Fatalf("link not reported: %v", res)
	}
	out := filepath.Join(t.TempDir(), "out")
	call(t, New(), opExtractArchive, map[string]any{"input_path": archive, "output_path": out})
	info, err := os.Stat(filepath.Join(out, "d", "b.txt"))
	if err != nil || !info.ModTime().Equal(when) {
		t.Fatalf("modification time %v, want %v (%v)", info.ModTime(), when, err)
	}
	if _, err := os.Lstat(filepath.Join(out, "link.txt")); err == nil {
		t.Fatal("a link was archived")
	}
}

func toStrings(v any) []string {
	var out []string
	if list, ok := v.([]any); ok {
		for _, s := range list {
			out = append(out, s.(string))
		}
	}
	return out
}

func TestWorkDirRules(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{"a.txt": "a"})
	work := t.TempDir()
	res := call(t, New(), opCreateArchive, map[string]any{"input_path": src, "output_path": filepath.Join(t.TempDir(), "a.duesarc"), "work_dir": work})
	if res["work_dir"] != work {
		t.Fatalf("work_dir %v", res["work_dir"])
	}
	if _, rerr := handle(New(), opCreateArchive, map[string]any{"input_path": src, "output_path": filepath.Join(t.TempDir(), "b.duesarc"), "work_dir": work}); rerr == nil {
		t.Fatal("a work_dir already holding an archive's database was reused")
	}
}

func TestSweepStaleWorkDirs(t *testing.T) {
	tmp := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	mk := func(name string) string {
		p := filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Join(p, "db"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stale := mk("duesarc-work-1")
	held := mk("duesarc-extract-2")
	fresh := mk("duesarc-work-3")
	other := mk("unrelated-4")
	w := &workDir{path: held}
	lock, err := lockWorkDir(held)
	if err != nil {
		t.Fatal(err)
	}
	w.lock = lock
	defer w.release(false)
	for _, p := range []string{stale, held, other} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	n, err := SweepStaleWorkDirs(tmp, 24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("swept %d, %v; want 1", n, err)
	}
	for p, want := range map[string]bool{stale: false, held: true, fresh: true, other: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", filepath.Base(p), err == nil, want)
		}
	}
}

// An encrypted archive's manifest.json is readable without the password, so
// it lists no paths, sizes or times; the full manifest is in the encrypted
// database, and extraction follows that copy even when manifest.json is
// edited to list other entries.
func TestEncryptedArchiveHidesItsPaths(t *testing.T) {
	src := filepath.Join(t.TempDir(), "case")
	files := map[string]string{
		"secret-name-one.txt":          "first",
		"dir-secret/inner-secret.bin":  strings.Repeat("x", 70_000),
		"dir-secret/copy-of-first.txt": "first",
		"empty-secret-dir/":            "",
	}
	writeTree(t, src, files)
	archive := filepath.Join(t.TempDir(), "case.duesarc")
	call(t, New(), opCreateArchive, map[string]any{"input_path": src, "output_path": archive, "password": "pw"})

	zr, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range []string{"secret-name-one", "inner-secret", "dir-secret", "copy-of-first", "empty-secret-dir"} {
			if strings.Contains(f.Name, needle) || bytes.Contains(data, []byte(needle)) {
				t.Fatalf("%s in the archive holds %q in the clear", f.Name, needle)
			}
		}
		if f.Name == manifestFileName {
			var m archiveManifest
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatal(err)
			}
			if !m.Encrypted || len(m.Entries) > 0 || m.InputBytes != 0 {
				t.Fatalf("manifest.json of an encrypted archive: %s", data)
			}
		}
	}
	zr.Close()

	if _, rerr := handle(New(), opListArchive, map[string]any{"input_path": archive}); rerr == nil || rerr.Details["reason"] != "password_required" {
		t.Fatalf("list without the password: %v", rerr)
	}
	out := filepath.Join(t.TempDir(), "out")
	call(t, New(), opExtractArchive, map[string]any{"input_path": archive, "output_path": out, "password": "pw"})
	sameTree(t, readTree(t, out), files)

	// manifest.json edited to list a path of its own: the database's copy
	// still decides what comes back.
	editManifest(t, archive, func(m *archiveManifest) {
		m.Encrypted = false
		m.Entries = []manifestEntry{{Path: "planted.txt", Type: entryTypeDir}}
	})
	out = filepath.Join(t.TempDir(), "out2")
	call(t, New(), opExtractArchive, map[string]any{"input_path": archive, "output_path": out, "password": "pw"})
	sameTree(t, readTree(t, out), files)
}

// TestDatabasePackageIsRefused: a database package (manifest version 3,
// kind database) is not an archive of files; list, verify and extract each
// refuse it and name the dues commands that read it.
func TestDatabasePackageIsRefused(t *testing.T) {
	pkg := filepath.Join(t.TempDir(), "case.duesarc")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range map[string]string{
		"db/dues-manifest.json": "{}",
		duesarc.ManifestName:    `{"version":"v3","kind":"database","db_path":"db"}`,
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(data))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pkg, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{opListArchive, opVerifyArchive, opExtractArchive} {
		params := map[string]any{"input_path": pkg}
		if op == opExtractArchive {
			params["output_path"] = filepath.Join(t.TempDir(), "out")
		}
		_, rerr := handle(New(), op, params)
		if rerr == nil || rerr.Code != "invalid_archive" || !strings.Contains(rerr.Message, "dues package import") {
			t.Errorf("%s: %v", op, rerr)
		}
	}
}
