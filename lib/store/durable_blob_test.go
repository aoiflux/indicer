package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/fio"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// chunkPassFunc runs one chunk pipeline.
type chunkPassFunc = func(context.Context, ChunkPass) (ChunkResult, error)

// The chunk passes a durable-blob test runs, by name.
var chunkPasses = map[string]chunkPassFunc{
	"batch":  ingestChunksBatchOwner,
	"staged": ingestChunksStaged,
}

// runChunkPass stores content as the evidence fhash through pass, and
// returns the evidence ID its relations are keyed by.
func runChunkPass(t testing.TB, pass chunkPassFunc, db *badger.DB, fhash string, content []byte) []byte {
	t.Helper()
	return runPass(t, pass, ChunkPass{DB: db, ID: []byte(fhash), Source: byteReaderAt(content), Size: int64(len(content)), Quiet: true})
}

// runPass runs the chunk pass p with pass and returns its ID.
func runPass(t testing.TB, pass chunkPassFunc, p ChunkPass) []byte {
	t.Helper()
	if err := util.EnsureBlobPath(p.DB.Opts().Dir); err != nil {
		t.Fatal(err)
	}
	if _, err := pass(t.Context(), p); err != nil {
		t.Fatalf("chunk pass for %s: %v", p.ID, err)
	}
	return p.ID
}

// blobFiles lists the chunk blobs in the database's blob directory.
func blobFiles(t *testing.T, db *badger.DB) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(util.BlobPath(db.Opts().Dir), "*"+cnst.BLOBEXT))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

// fileModes are the storage modes that keep one blob file per chunk.
var fileModes = []testdb.Mode{testdb.Default, testdb.Encrypted, testdb.Quick}

// TestReingestRewritesTruncatedBlob is the STO-4 regression. A chunk blob was
// created in place with O_EXCL, and any existing blob counted as valid, so a
// blob a crash or a full disk left short was trusted by every later ingest
// sharing the chunk, and the evidence could never be restored.
func TestReingestRewritesTruncatedBlob(t *testing.T) {
	for name, pass := range chunkPasses {
		for _, m := range fileModes {
			t.Run(name+"/"+m.Name, func(t *testing.T) {
				db := testdb.Open(t, m)
				util.SetChonkSize(4)
				cs := int(cnst.ChonkSize)
				content := testutil.RandomBytes(7, 3*cs)

				runChunkPass(t, pass, db.DB, "first", content)
				blobs := blobFiles(t, db.DB)
				if len(blobs) != 3 {
					t.Fatalf("first pass wrote %d blobs, want 3", len(blobs))
				}
				for _, p := range blobs {
					fi, err := os.Stat(p)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.Truncate(p, fi.Size()/2); err != nil {
						t.Fatal(err)
					}
				}

				runChunkPass(t, pass, db.DB, "second", content)
				for i := 0; i < 3; i++ {
					chunk := content[i*cs : (i+1)*cs]
					chash, err := util.GetChonkHash(chunk, cnst.GetHashAlgo())
					if err != nil {
						t.Fatal(err)
					}
					got, err := dbio.GetChonkNode(util.AppendToBytesSlice(cnst.ChonkNamespace, chash), db.DB)
					if err != nil {
						t.Fatalf("chunk %d after re-ingest over a truncated blob: %v", i, err)
					}
					if !bytes.Equal(got, chunk) {
						t.Fatalf("chunk %d after re-ingest: %d bytes differ from the %d stored", i, len(got), len(chunk))
					}
				}
				assertNoBlobTemps(t, db.DB)
			})
		}
	}
}

// TestReingestSkipsStoredChunks is the STO-23 regression. Re-ingesting
// content whose chunks were already stored compressed and encrypted every
// chunk again, only for the write to find the blob present.
func TestReingestSkipsStoredChunks(t *testing.T) {
	for name, pass := range chunkPasses {
		for _, m := range append(fileModes, testdb.Container) {
			t.Run(name+"/"+m.Name, func(t *testing.T) {
				db := testdb.Open(t, m)
				util.SetChonkSize(4)
				content := testutil.RandomBytes(8, 3*int(cnst.ChonkSize)+17)
				runChunkPass(t, pass, db.DB, "first", content)

				before := map[string]os.FileInfo{}
				for _, p := range blobFiles(t, db.DB) {
					fi, err := os.Stat(p)
					if err != nil {
						t.Fatal(err)
					}
					before[p] = fi
				}

				prev := materializeChunk
				t.Cleanup(func() { materializeChunk = prev })
				var calls atomic.Int64
				materializeChunk = func(key, data []byte, db *badger.DB, c *fio.ContainerManager) ([]byte, error) {
					calls.Add(1)
					return prev(key, data, db, c)
				}

				runChunkPass(t, pass, db.DB, "second", content)
				if n := calls.Load(); n != 0 {
					t.Fatalf("re-ingest materialised %d chunks already stored, want 0", n)
				}
				for p, fi := range before {
					now, err := os.Stat(p)
					if err != nil {
						t.Fatal(err)
					}
					if !os.SameFile(fi, now) || now.Size() != fi.Size() {
						t.Fatalf("re-ingest rewrote the intact blob %s", filepath.Base(p))
					}
				}
			})
		}
	}
}

// assertNoBlobTemps fails if a blob write left a temporary file behind.
func assertNoBlobTemps(t *testing.T, db *badger.DB) {
	t.Helper()
	entries, err := os.ReadDir(util.BlobPath(db.Opts().Dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), cnst.BLOBEXT) && !e.IsDir() {
			t.Fatalf("blob directory holds a stray file %s", e.Name())
		}
	}
}
