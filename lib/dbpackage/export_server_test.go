package dbpackage

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
)

// derivedSize is what a package of root holds of its full-text index and
// graph.
func derivedSize(t *testing.T, root string) uint64 {
	t.Helper()
	var n uint64
	for _, d := range []string{cnst.FTSDIR, cnst.GRAPHDIR} {
		err := filepath.WalkDir(filepath.Join(root, d), func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			if e.IsDir() || skip(filepath.ToSlash(rel), false, false) {
				return nil
			}
			info, err := e.Info()
			if err != nil {
				return err
			}
			n += uint64(info.Size())
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return n
}

// TestEstimateNeedCoversPackage: the room a server checks for before it
// suspends a database covers the package it then writes; what packages
// leave out does not count, and without the derived parts it is less by at
// least their size.
func TestEstimateNeedCoversPackage(t *testing.T) {
	t.Parallel()
	root := newRoot(t, dbmeta.NoPassword())
	need, err := EstimateNeed(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{cnst.ExportsDir, cnst.UploadsDir} {
		p := filepath.Join(root, d, "left-out.bin")
		if err := os.MkdirAll(filepath.Dir(p), cnst.DirPerm); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, evidenceBytes), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if again, err := EstimateNeed(t.Context(), root, false); err != nil || again != need {
		t.Errorf("with files in %s and %s: %d (%v), want %d", cnst.ExportsDir, cnst.UploadsDir, again, err, need)
	}
	lean, err := EstimateNeed(t.Context(), root, true)
	if err != nil {
		t.Fatal(err)
	}
	if d := derivedSize(t, root); d == 0 || need < lean || need-lean < d {
		t.Errorf("without derived parts %d, whole %d; the derived parts hold %d", lean, need, d)
	}
	res := export(t, ExportOptions{Path: root})
	if need < uint64(res.Bytes) {
		t.Errorf("estimated %d bytes, the package holds %d", need, res.Bytes)
	}
	if _, err := EstimateNeed(t.Context(), filepath.Join(t.TempDir(), "missing"), false); err == nil {
		t.Error("a root that does not exist was sized")
	}
}

// TestEstimateNeedBatches: a folder larger than one read batch is sized
// whole, and a sizing whose context ended stops instead of reading on.
func TestEstimateNeedBatches(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	blobs := filepath.Join(root, cnst.BLOBSDIR)
	if err := os.Mkdir(blobs, cnst.DirPerm); err != nil {
		t.Fatal(err)
	}
	const files = sizingBatch + 1
	for i := range files {
		if err := os.WriteFile(filepath.Join(blobs, strconv.Itoa(i)), []byte{1}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// One byte per file, and one entry for the folder besides.
	if need, err := EstimateNeed(t.Context(), root, false); err != nil || need != spaceNeed(files, files+1, 0) {
		t.Errorf("sized %d (%v), want %d", need, err, spaceNeed(files, files+1, 0))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := EstimateNeed(ctx, root, false); !errors.Is(err, context.Canceled) {
		t.Errorf("a sizing whose context ended: %v", err)
	}
}

// TestCheckOutputDir: packages may go outside the root, or into its
// exports folder, spelled exactly, once it exists; nowhere else in it.
func TestCheckOutputDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exports := filepath.Join(root, cnst.ExportsDir)
	if inRoot, err := CheckOutputDir(root, exports); !isUsage(err) || inRoot {
		t.Errorf("the exports folder before it exists: %v, %v", inRoot, err)
	}
	sub := filepath.Join(exports, "sub")
	if err := os.MkdirAll(sub, cnst.DirPerm); err != nil {
		t.Fatal(err)
	}
	for dir, wantIn := range map[string]bool{t.TempDir(): false, exports: true, sub: true} {
		if inRoot, err := CheckOutputDir(root, dir); err != nil || inRoot != wantIn {
			t.Errorf("%s: %v, %v; want in root %v", dir, inRoot, err, wantIn)
		}
	}
	for _, dir := range []string{root, filepath.Join(root, cnst.BLOBSDIR), filepath.Join(root, cnst.ExportsDir+"-old")} {
		if inRoot, err := CheckOutputDir(root, dir); !isUsage(err) || inRoot || !strings.Contains(err.Error(), dir) {
			t.Errorf("%s: %v, %v; want a usage error naming it", dir, inRoot, err)
		}
	}

	// Packages leave out only a folder spelled exactly so.
	other := t.TempDir()
	upper := filepath.Join(other, strings.ToUpper(cnst.ExportsDir[:1])+cnst.ExportsDir[1:])
	if err := os.Mkdir(upper, cnst.DirPerm); err != nil {
		t.Fatal(err)
	}
	if inRoot, err := CheckOutputDir(other, upper); !isUsage(err) || inRoot {
		t.Errorf("%s: %v, %v; want a usage error", upper, inRoot, err)
	}
}
