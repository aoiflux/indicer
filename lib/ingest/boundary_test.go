package ingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/dbio"
	"indicer/lib/store"

	"github.com/dgraph-io/badger/v4"
)

// TestChunkBoundaryMatrix stores files whose sizes sit on and around chunk
// boundaries, at chunk sizes from 4 KiB to 64 MiB, and reads each back whole
// (restore, with every chunk and the whole hash verified) and as ranges that
// start before, on and after a boundary and end anywhere. 64 MiB chunks make
// files of up to 192 MiB, so that size runs only as a long test.
func TestChunkBoundaryMatrix(t *testing.T) {
	for _, kib := range []int{4, 64, 1024, 65536} {
		t.Run(fmt.Sprintf("%dKiB", kib), func(t *testing.T) {
			if kib >= 65536 {
				testutil.LongTest(t)
			}
			db := openDBWith(t, testdb.Options{ChunkKiB: kib})
			cs := kib << 10
			dir := t.TempDir()
			for i, size := range []int{0, 1, cs - 1, cs, cs + 1, 3*cs - 7, 3 * cs, 3*cs + 7} {
				data := testutil.RandomBytes(uint64(1000+i), size)
				path := filepath.Join(dir, fmt.Sprintf("f%d.bin", i))
				if err := os.WriteFile(path, data, 0o644); err != nil {
					t.Fatal(err)
				}
				res, err := Run(t.Context(), db, Request{Path: path, Decode: true, NoIndex: true, Quiet: true})
				if err != nil {
					t.Fatalf("size %d: %v", size, err)
				}
				if res.Outcome != store.CommitStored {
					t.Fatalf("size %d: outcome %v", size, res.Outcome)
				}

				var out bytes.Buffer
				rep, err := store.Restore(t.Context(), db, base64.StdEncoding.EncodeToString(res.EvidenceID), &out, store.RestoreOptions{Verify: store.VerifyFull})
				if err != nil {
					t.Fatalf("size %d: restore: %v", size, err)
				}
				if sha256.Sum256(out.Bytes()) != sha256.Sum256(data) {
					t.Fatalf("size %d: restored %d bytes of other content", size, out.Len())
				}
				if size > 0 && rep.Digest != store.DigestVerified {
					t.Fatalf("size %d: digest %v", size, rep.Digest)
				}
				if size > 0 {
					checkRanges(t, db, res.EvidenceID, data, cs)
				}
			}
		})
	}
}

// checkRanges reads ranges of the evidence through the reader and compares
// them with its content.
func checkRanges(t *testing.T, db *badger.DB, eid []byte, data []byte, cs int) {
	t.Helper()
	ev, err := dbio.OpenEvidenceChunks(db, eid, dbio.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	size := len(data)
	for _, start := range []int{0, 1, cs - 1, cs, cs + 1, 2*cs - 1, size - 1} {
		if start < 0 || start >= size {
			continue
		}
		for _, n := range []int{1, cs - 1, cs, cs + 2, 2*cs + 3, size} {
			n = min(n, size-start)
			if n <= 0 {
				continue
			}
			buf := make([]byte, n)
			got, err := ev.ReadAt(buf, int64(start))
			if err != nil || got != n || !bytes.Equal(buf, data[start:start+n]) {
				t.Fatalf("size %d, %d-byte chunks: ReadAt(%d bytes at %d) = %d, %v, equal %v",
					size, cs, n, start, got, err, bytes.Equal(buf[:got], data[start:start+got]))
			}
		}
	}
	// A read past the end is short, never invented.
	buf := make([]byte, 10)
	from := max(size-3, 0)
	if got, _ := ev.ReadAt(buf, int64(from)); got != size-from || !bytes.Equal(buf[:got], data[from:]) {
		t.Fatalf("size %d: read across the end returned %d bytes, want %d", size, got, size-from)
	}
}
