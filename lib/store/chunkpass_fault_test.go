//go:build faultinject

package store

import (
	"errors"
	"testing"

	"indicer/internal/faultinject"
	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/util"
)

// TestWriterErrorIsNotMasked: when the writer fails mid-pass (here its
// durability barrier), the pass reports the writer's error, not the
// cancellation it caused in the hash workers.
func TestWriterErrorIsNotMasked(t *testing.T) {
	for name, pass := range chunkPasses {
		t.Run(name, func(t *testing.T) {
			db := testdb.Open(t, testdb.Default)
			util.SetChonkSize(4)
			if err := util.EnsureBlobPath(db.DB.Opts().Dir); err != nil {
				t.Fatal(err)
			}
			// More chunks than the barrier holds, so it fires mid-pass.
			content := testutil.RandomBytes(11, 32<<20)
			restore, err := faultinject.Set(faultinject.StoreChunkBeforeBarrier + "=error")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(restore)
			_, err = pass(t.Context(), ChunkPass{DB: db.DB, ID: []byte("writer-fault"), Source: byteReaderAt(content), Size: int64(len(content)), Quiet: true})
			if !errors.Is(err, faultinject.ErrInjected) {
				t.Fatalf("pass error %v, want the writer's injected fault", err)
			}
		})
	}
}
