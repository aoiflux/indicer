package index

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"math/rand/v2"
	"testing"

	"indicer/internal/testutil/fsfixture"
	"indicer/lib/dbio"
	"indicer/lib/evidence"
	"indicer/lib/store"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// storeEvidence stores img as the evidence testEID and marks it completed,
// so its indexed files can be read and restored.
func storeEvidence(t *testing.T, db *badger.DB, img io.ReaderAt, size int64, parallel bool, source *structs.EvidenceSource) {
	t.Helper()
	if _, err := store.IngestChunks(t.Context(), store.ChunkPass{DB: db, ID: testEID, Source: img, Size: size, Parallel: parallel, Quiet: true}); err != nil {
		t.Fatal(err)
	}
	err := dbio.UpdateEvidence(db, testEID, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		e.Completed, e.IngestState, e.Source = true, structs.IngestStateCompleted, source
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// checkReader reads every indexed record through the reader restore uses,
// and restores up to maxRestores of them, comparing both with the content
// read from the image itself.
func checkReader(t *testing.T, db *badger.DB, img io.ReaderAt, recs map[string][]indexed, maxRestores int) {
	t.Helper()
	restores := 0
	for path, rs := range recs {
		for _, r := range rs {
			key := dbio.CanonicalIndexedKey(r.id)
			l, err := dbio.OpenLogical(db, key, dbio.ReadOptions{})
			if r.rec.Content.Kind == structs.ContentUnavailable {
				if !errors.Is(err, dbio.ErrContentUnavailable) {
					t.Errorf("%s: open of a metadata-only record: %v, want ErrContentUnavailable", path, err)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s: %v", path, err)
				continue
			}
			want := contentOf(t, db, img, r.id, r.rec)
			if l.Size() != int64(len(want)) || l.RecordedSize() != r.rec.Size || l.Truncated() != (r.rec.Content.Located < r.rec.Size) {
				t.Errorf("%s: reader size %d (recorded %d, truncated %v), want %d of %d", path, l.Size(), l.RecordedSize(), l.Truncated(), len(want), r.rec.Size)
				continue
			}
			var streamed []byte
			if err := l.Stream(t.Context(), func(b []byte) error { streamed = append(streamed, b...); return nil }); err != nil || !bytes.Equal(streamed, want) {
				t.Errorf("%s: streamed %d bytes, %v; content differs", path, len(streamed), err)
				continue
			}
			if len(want) > 0 {
				rng := rand.New(rand.NewPCG(uint64(len(want)), 7))
				for range 4 {
					off := rng.IntN(len(want))
					n := 1 + rng.IntN(len(want)-off)
					p := make([]byte, n)
					if m, err := l.ReadAt(p, int64(off)); m != n || (err != nil && !errors.Is(err, io.EOF)) || !bytes.Equal(p, want[off:off+n]) {
						t.Errorf("%s: ReadAt(%d, %d) = %d, %v; content differs", path, n, off, m, err)
					}
				}
			}
			if restores >= maxRestores && r.rec.Content.Kind == structs.ContentExtents {
				continue
			}
			restores++
			var out bytes.Buffer
			rep, err := store.Restore(t.Context(), db, base64.StdEncoding.EncodeToString(r.id), &out, store.RestoreOptions{Verify: store.VerifyFull})
			switch {
			case err != nil:
				t.Errorf("%s: restore: %v", path, err)
			case !bytes.Equal(out.Bytes(), want):
				t.Errorf("%s: restored %d bytes, content differs", path, out.Len())
			case rep.Digest != store.DigestVerified || rep.Truncated != l.Truncated() || rep.RecordedSize != r.rec.Size:
				t.Errorf("%s: restore report %+v", path, rep)
			}
		}
	}
}

func TestReaderContentKinds(t *testing.T) {
	db, img, recs, _ := indexSynthetic(t)
	storeEvidence(t, db, img, img.Size(), false, nil)
	checkReader(t, db, img, recs, 1<<30)
	if r := recs["/truncated.bin"][0]; r.rec.Content.Located == r.rec.Size {
		t.Fatal("the synthetic set has no truncated file")
	}
}

func TestReaderServesFixtures(t *testing.T) {
	for _, m := range fsfixture.All(t) {
		t.Run(m.Name, func(t *testing.T) {
			src, err := evidence.Open(t.Context(), fsfixture.Open(t, m), evidence.Options{Decode: true})
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			img := src.Image()
			db := openIndexDB(t, img.Size())
			res, err := Run(t.Context(), Options{DB: db, EvidenceID: testEID, EvidenceName: "ev", Image: img})
			if err != nil {
				t.Fatal(err)
			}
			info := src.Info()
			storeEvidence(t, db, img, img.Size(), src.Parallel(), &info)
			all := map[string][]indexed{}
			for _, recs := range recordsByPartition(t, db, testEID) {
				for p, rs := range recs {
					all[p] = append(all[p], rs...)
				}
			}
			checkReader(t, db, img, all, 24)

			// A partition reads as its range of the evidence.
			for _, p := range res.Partitions {
				l, err := dbio.OpenLogical(db, dbio.CanonicalPartitionKey(p.ID), dbio.ReadOptions{})
				if err != nil {
					t.Fatalf("partition %x: %v", p.ID, err)
				}
				got := make([]byte, min(l.Size(), 1<<20))
				want := make([]byte, len(got))
				if _, err := l.ReadAt(got, 0); err != nil && !errors.Is(err, io.EOF) {
					t.Fatal(err)
				}
				if _, err := img.ReadAt(want, p.Offset); err != nil && !errors.Is(err, io.EOF) {
					t.Fatal(err)
				}
				if l.Size() != p.Size || !bytes.Equal(got, want) {
					t.Errorf("partition at %d: reads %d bytes, want %d; content differs", p.Offset, l.Size(), p.Size)
				}
			}

			if info.EWF == nil {
				return
			}
			// An EWF evidence restores as the decoded image, checked against
			// the digests the set recorded.
			ekey := dbio.CanonicalEvidenceKey(testEID)
			b64 := base64.StdEncoding.EncodeToString(testEID)
			var out bytes.Buffer
			rep, err := store.Restore(t.Context(), db, b64, &out, store.RestoreOptions{Verify: store.VerifyChunks, VerifyEWF: true})
			if err != nil || rep.EWF != structs.EWFVerified || int64(out.Len()) != img.Size() {
				t.Fatalf("EWF restore: %v, report %+v, %d bytes", err, rep, out.Len())
			}
			bad := *info.EWF
			bad.StoredMD5 = bytes.Repeat([]byte{0xee}, 16)
			badSource := info
			badSource.EWF = &bad
			if err := dbio.UpdateEvidence(db, ekey, func(_ *badger.Txn, e *structs.EvidenceFile) error { e.Source = &badSource; return nil }); err != nil {
				t.Fatal(err)
			}
			rep, err = store.Restore(context.Background(), db, b64, io.Discard, store.RestoreOptions{Verify: store.VerifyChunks, VerifyEWF: true})
			if err != nil || rep.EWF != structs.EWFMismatch {
				t.Fatalf("EWF restore against a wrong stored MD5: %v, report %+v, want a mismatch", err, rep)
			}
		})
	}
}
