package store

import (
	"os"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/dbmeta"
	"indicer/lib/util"
)

// TestWrittenBytesAreTheBlobs: a chunk pass reports, as WrittenBytes, what
// its new chunks take on disk, and as NewBytes their raw size; a chunk the
// pass already stored, or one stored before it, counts in neither.
func TestWrittenBytesAreTheBlobs(t *testing.T) {
	for name, pass := range chunkPasses {
		for _, m := range fileModes {
			t.Run(name+"/"+m.Name, func(t *testing.T) {
				db := testdb.Open(t, m, testdb.Options{ChunkKiB: 4})
				cs := int(dbmeta.ChunkSize(db.DB))
				unique := testutil.RandomBytes(9, 3*cs)
				// The first chunk again, then zeros, which compress well.
				content := append(append(append([]byte{}, unique...), unique[:cs]...), make([]byte, cs)...)
				if err := util.EnsureBlobPath(db.DB.Opts().Dir); err != nil {
					t.Fatal(err)
				}
				res, err := pass(t.Context(), ChunkPass{DB: db.DB, ID: []byte("first"), Source: byteReaderAt(content), Size: int64(len(content)), Quiet: true})
				if err != nil {
					t.Fatal(err)
				}
				var onDisk int64
				for _, p := range blobFiles(t, db.DB) {
					fi, err := os.Stat(p)
					if err != nil {
						t.Fatal(err)
					}
					onDisk += fi.Size()
				}
				if res.NewBytes != int64(4*cs) || res.WrittenBytes != onDisk || onDisk == 0 {
					t.Fatalf("new %d, written %d; want new %d and written %d, the blobs' size", res.NewBytes, res.WrittenBytes, 4*cs, onDisk)
				}

				again, err := pass(t.Context(), ChunkPass{DB: db.DB, ID: []byte("second"), Source: byteReaderAt(content), Size: int64(len(content)), Quiet: true})
				if err != nil {
					t.Fatal(err)
				}
				if again.NewBytes != 0 || again.WrittenBytes != 0 {
					t.Fatalf("second pass: new %d, written %d, want 0 and 0", again.NewBytes, again.WrittenBytes)
				}
			})
		}
	}
}
