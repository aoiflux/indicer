package legacyfixture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/testutil"
	"indicer/lib/blobcodec"
	"indicer/lib/dbmeta"
)

// fileHashes maps every file under root to the SHA-256 of its content and
// its size. Lock files, which readers create to coordinate, are left out.
func fileHashes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(d.Name(), ".lock") || strings.Contains(d.Name(), ".lock.") {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = fmt.Sprintf("%s %d", hex.EncodeToString(h.Sum(nil)), fi.Size())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diffHashes(before, after map[string]string) []string {
	var d []string
	for k, v := range before {
		if got := after[k]; got != v {
			d = append(d, k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			d = append(d, k+" (new)")
		}
	}
	return d
}

// goodRestore returns a restore the writing release got right.
func goodRestore(f Fixture) (Restore, VolumeFile, bool) {
	for _, r := range f.Restores {
		want, ok := f.Expected(r)
		if ok && r.Status == "ok" && r.SHA256 != nil && *r.SHA256 == want.SHA256 {
			return r, want, true
		}
	}
	return Restore{}, VolumeFile{}, false
}

func mustRun(t *testing.T, dir, bin string, args ...string) string {
	t.Helper()
	out, err := runIn(t, dir, bin, args...)
	if err != nil {
		t.Fatalf("dues %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func restoreAndCheck(t *testing.T, f Fixture, bin, work, db, hash, out string, want VolumeFile) {
	t.Helper()
	mustRun(t, work, bin, append([]string{"restore", hash, "-f", out, "--dbpath", db}, f.ReadFlags(t)...)...)
	got, size, err := fileSHA256(out)
	if err != nil || got != want.SHA256 || size != want.Size {
		t.Fatalf("restore of %s: %d bytes, sha256 %s, %v; want %d bytes, %s", hash, size, got, err, want.Size, want.SHA256)
	}
}

// TestLegacyReadsChangeNothing: reading a database an earlier release wrote
// (listing, statistics, repair's inspection, a search, a restore) changes no
// file of it and writes no manifest: a read-only open adopts what it infers
// without persisting it.
func TestLegacyReadsChangeNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the dues binary")
	}
	bin := testutil.BuildDues(t)
	for _, f := range Load(t) {
		t.Run(f.Mode, func(t *testing.T) {
			db := Extract(t, f)
			work := t.TempDir()
			before := fileHashes(t, db)
			reads := [][]string{{"list"}, {"stats"}, {"repair"}, {"search", "abc", "--out", filepath.Join(work, "report.json")}}
			if r, _, ok := goodRestore(f); ok {
				reads = append(reads, []string{"restore", r.Hash, "-f", filepath.Join(work, "restored.bin")})
			}
			for _, args := range reads {
				args = append(append(args, "--dbpath", db), f.ReadFlags(t)...)
				out, err := runIn(t, work, bin, args...)
				var exit *exec.ExitError
				// A search reports what it could not read (the container
				// fixtures hold STO-1 damage) with exit code 2.
				if err != nil && !(args[0] == "search" && errors.As(err, &exit) && exit.ExitCode() == 2) {
					t.Fatalf("dues %s: %v\n%s", strings.Join(args, " "), err, out)
				}
			}
			if d := diffHashes(before, fileHashes(t, db)); len(d) > 0 {
				t.Fatalf("reading changed %v", d)
			}
			if _, err := os.Stat(dbmeta.ManifestPath(db)); err == nil {
				t.Fatal("a read wrote a manifest")
			}
		})
	}
}

// TestStoreIntoLegacyDatabase: new evidence stored into a database an
// earlier release wrote keeps that database's layout and encoding, and both
// it and the old evidence restore.
func TestStoreIntoLegacyDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the dues binary")
	}
	bin := testutil.BuildDues(t)
	for _, f := range Load(t) {
		t.Run(f.Mode, func(t *testing.T) {
			db := Extract(t, f)
			work := t.TempDir()
			src := testutil.WriteRandomFile(t, work, "new.bin", 99, 700<<10)
			mustRun(t, work, bin, append([]string{"store", src, "--dbpath", db}, f.ReadFlags(t)...)...)

			m, err := dbmeta.Load(db)
			if err != nil {
				t.Fatalf("the manifest a writable open records: %v", err)
			}
			if m.Inferred == nil || m.Layout.BlobEncoding == blobcodec.V2 {
				t.Fatalf("manifest %+v: a legacy database must keep its encoding", m.Layout)
			}

			out := mustRun(t, work, bin, append([]string{"list", "--dbpath", db}, f.ReadFlags(t)...)...)
			var doc struct {
				Evidence []struct {
					Name, Hash, Status string
				} `json:"evidence"`
			}
			i, j := strings.Index(out, "{"), strings.LastIndex(out, "}")
			if i < 0 || j < i || json.Unmarshal([]byte(out[i:j+1]), &doc) != nil {
				t.Fatalf("list:\n%s", out)
			}
			var newHash string
			for _, e := range doc.Evidence {
				if e.Name == "new.bin" && e.Status == "completed" {
					newHash = e.Hash
				}
			}
			if newHash == "" {
				t.Fatalf("new.bin is not listed as completed:\n%s", out)
			}
			restoreAndCheck(t, f, bin, work, db, newHash, filepath.Join(work, "new.out"),
				VolumeFile{SHA256: testutil.FileSHA256(t, src), Size: 700 << 10})
			if r, want, ok := goodRestore(f); ok {
				restoreAndCheck(t, f, bin, work, db, r.Hash, filepath.Join(work, "old.out"), want)
			}
		})
	}
}

// TestLegacyE01AfterUpgrade: the release that wrote the e01 fixture stored
// the E01 file's own bytes and failed to index it, so it holds no index to
// quarantine (TestQuarantineRawContainerIndexes covers one that does).
// `repair --upgrade` leaves it as it was: it restores to the E01 file.
func TestLegacyE01AfterUpgrade(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the dues binary")
	}
	bin := testutil.BuildDues(t)
	for _, f := range Load(t) {
		if f.Mode != "e01" {
			continue
		}
		db := Extract(t, f)
		work := t.TempDir()
		mustRun(t, work, bin, append([]string{"repair", "--upgrade", "--yes", "--dbpath", db}, f.ReadFlags(t)...)...)
		r, want, ok := goodRestore(f)
		if !ok {
			t.Fatal("the e01 fixture has no restore its release got right")
		}
		restoreAndCheck(t, f, bin, work, db, r.Hash, filepath.Join(work, "e01.out"), want)
		return
	}
	t.Fatal("no e01 fixture")
}
