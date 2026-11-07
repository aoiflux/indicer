package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/util"
)

// TestStagedPipelineRoundTrip is the STO-9 and CFG-6 regression for the
// staged store pipeline. Its dedup stage dropped a chunk already seen in the
// session before the chunk's relation was written, so evidence with repeated
// content could not be restored. Its hash stage always used BLAKE3, whatever
// chunk hash the database records, so its chunk keys never matched those the
// default pipeline writes.
func TestStagedPipelineRoundTrip(t *testing.T) {
	cs := matrixChunkKB * 1024
	for _, algo := range []string{cnst.BLAKE3, cnst.SHA3} {
		t.Run(algo, func(t *testing.T) {
			testdb.ForEachMode(t, func(t *testing.T, m testdb.Mode) {
				cnst.StorePipelineMode = cnst.StorePipelineStaged
				dir := t.TempDir()
				dbPath := filepath.Join(dir, "db")
				opts := optsForMode(dbPath, matrixChunkKB, m)
				opts.Layout.ChunkHash = algo

				// Chunks A B A C A, then a short tail: A repeats within the session.
				a := testutil.RandomBytes(1, cs)
				b := testutil.RandomBytes(2, cs)
				c := testutil.RandomBytes(3, cs)
				content := bytes.Join([][]byte{a, b, a, c, a, testutil.RandomBytes(4, 17)}, nil)
				src := filepath.Join(dir, "evidence.bin")
				if err := os.WriteFile(src, content, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := StoreData(t.Context(), opts, src, false, true, false, false); err != nil {
					t.Fatalf("store: %v", err)
				}

				out := filepath.Join(dir, "restored.bin")
				if err := RestoreData(t.Context(), opts, fileHashB64(t, src, algo), out, RestoreOptions{}); err != nil {
					t.Fatalf("restore: %v", err)
				}
				got, err := os.ReadFile(out)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, content) {
					t.Fatalf("restored %d bytes differ from the %d stored", len(got), len(content))
				}

				h, err := openTestDB(dbPath, []byte(m.Password))
				if err != nil {
					t.Fatal(err)
				}
				defer h.Close()
				eid, err := dbio.GuessFileType(fileHashB64(t, src, algo), h.DB)
				if err != nil {
					t.Fatal(err)
				}
				for i, chunk := range [][]byte{a, b, a, c, a} {
					rel := util.AppendToBytesSlice(cnst.RelationNamespace, dbio.EvidenceRawID(eid), cnst.DataSeperator, int64(i*cs))
					stored, err := dbio.GetNode(rel, h.DB)
					if err != nil {
						t.Fatalf("relation for chunk %d: %v", i, err)
					}
					want, err := util.GetChonkHash(chunk, dbmeta.ChunkHasher(h.DB)())
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(stored, want) {
						t.Fatalf("chunk %d is keyed by a %d-byte hash, not the database's %s chunk hash", i, len(stored), algo)
					}
				}
			})
		})
	}
}
