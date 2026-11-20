package fio

import (
	"os"
	"path/filepath"
	"testing"

	"indicer/internal/testutil"
	"indicer/lib/cnst"
)

// FuzzScanContainer scans container files it did not write: every frame it
// reports lies inside the file, and nothing panics or allocates past it.
func FuzzScanContainer(f *testing.F) {
	dir := newContainerDB(f)
	cm := NewContainerManager(dir)
	chunks := writeChunks(f, cm, 0, 3)
	closeManager(f, cm)
	seed, err := os.ReadFile(chunks[0].path)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add(seed[:len(seed)/2])
	f.Add([]byte{})
	// One file, rewritten by each input: a fresh directory per input is
	// slower than the scan.
	path := filepath.Join(f.TempDir(), "ctr.blob")
	f.Fuzz(func(t *testing.T, data []byte) {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		_ = ScanContainer(path, func(fr ContainerFrame) error {
			if fr.Offset < 0 || fr.Size < 0 || fr.Offset+fr.Size > int64(len(data)) {
				t.Fatalf("frame at %d+%d in a %d-byte file", fr.Offset, fr.Size, len(data))
			}
			return nil
		})
	})
}

// FuzzLookupBlockIndex looks chunks up in block index files it did not
// write: an error or a location, never a panic or an allocation the file's
// size does not bound.
func FuzzLookupBlockIndex(f *testing.F) {
	hashes := [][]byte{make([]byte, 32), append([]byte{0x00, 1}, make([]byte, 30)...)}
	hashes[0][31] = 9
	seedDir := f.TempDir()
	recs := []testutil.LegacyBlockRecord{
		{Hash: hashes[0], Path: "blob/ctr-a.blob", Offset: 64, Size: 100},
		{Hash: hashes[1], Path: "blob/ctr-b.blob", Offset: 4096, Size: 7},
	}
	if err := testutil.WriteLegacyBlockIndex(filepath.Join(seedDir, cnst.BLOBSDIR), recs); err != nil {
		f.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(seedDir, cnst.BLOBSDIR, "blocks", "*.bidx"))
	if len(files) == 0 {
		f.Fatal("no block files written")
	}
	seed, err := os.ReadFile(files[0])
	if err != nil {
		f.Fatal(err)
	}
	name := filepath.Base(files[0])
	f.Add(seed)
	f.Add(seed[:len(seed)-3])
	db := f.TempDir()
	dir := filepath.Join(db, cnst.BLOBSDIR, "blocks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		for _, h := range hashes {
			_, off, size, err := LookupBlockIndex(db, append([]byte(cnst.ChonkNamespace), h...))
			if err == nil && (off < 0 || size < 0) {
				t.Fatalf("location %d+%d", off, size)
			}
		}
	})
}
