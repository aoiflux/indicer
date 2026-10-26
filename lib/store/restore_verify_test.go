package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/blobcodec"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// seedCompletedEvidence stores content as a completed evidence whose ID is
// id and whose recorded hash is fileHash, and returns the ID its relations
// are keyed by.
func seedCompletedEvidence(t testing.TB, db *badger.DB, id string, content []byte, fileHash string) []byte {
	t.Helper()
	rid := runChunkPass(t, ingestChunksBatchOwner, db, id, content)
	ef := structs.NewEvidenceFile("evidence-"+id, 0, int64(len(content)), nil, "dd", fileHash)
	ef.Completed = true
	ef.IngestState = structs.IngestStateCompleted
	if err := dbio.SetFile(util.AppendToBytesSlice(cnst.EviFileNamespace, rid), ef, db); err != nil {
		t.Fatalf("seed evidence %s: %v", id, err)
	}
	return rid
}

// seedPartition records [start, start+size) of the evidence rid as a
// partition with the recorded hash fileHash, and returns its restore hash.
func seedPartition(t testing.TB, db *badger.DB, rid []byte, name string, start, size int64, fileHash string) string {
	t.Helper()
	raw := bytes.Repeat([]byte{byte(len(name))}, 16)
	copy(raw, name)
	p := structs.NewPartitionFile(base64.StdEncoding.EncodeToString(rid)+cnst.DataSeperator+name, start, size, nil, fileHash)
	if err := dbio.SetFile(util.AppendToBytesSlice(cnst.PartiFileNamespace, raw), p, db); err != nil {
		t.Fatalf("seed partition %s: %v", name, err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func restoreBytes(t *testing.T, db *badger.DB, rhash string, opts RestoreOptions) ([]byte, RestoreReport, error) {
	t.Helper()
	var out bytes.Buffer
	report, err := Restore(t.Context(), db, rhash, &out, opts)
	return out.Bytes(), report, err
}

func plainHash(data []byte) string {
	h := cnst.GetHashAlgo(true)
	h.Write(data)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// evidenceHocho is the hocho evidence hash as ingest computes it: the file
// hash over each chunk hash, length first.
func evidenceHocho(content []byte, cs int64) string {
	h := cnst.GetHashAlgo(true)
	for off := int64(0); off < int64(len(content)); off += cs {
		ch := cnst.GetHashAlgo()
		ch.Write(content[off:min(int64(len(content)), off+cs)])
		sum := ch.Sum(nil)
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(sum)))
		h.Write(n[:])
		h.Write(sum)
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// TestRestoreUsesEachEvidenceChunkSize is the RST-2 regression. Restore
// stepped through an evidence by the process's chunk size, so in a legacy
// database holding evidence stored with different chunk sizes only the
// evidence matching the current setting restored. The chunk size of each
// evidence now comes from its own relations.
func TestRestoreUsesEachEvidenceChunkSize(t *testing.T) {
	db := testdb.Open(t, testdb.Default)

	util.SetChonkSize(4)
	a := testutil.RandomBytes(1, 5*4096+7)
	ridA := seedCompletedEvidence(t, db.DB, "evidence-at-4kib", a, plainHash(a))
	pA := seedPartition(t, db.DB, ridA, "part-a", 3*4096-10, 100, "")

	util.SetChonkSize(8)
	b := testutil.RandomBytes(2, 3*8192+5)
	ridB := seedCompletedEvidence(t, db.DB, "evidence-at-8kib", b, plainHash(b))

	for _, c := range []struct {
		name, hash string
		want       []byte
	}{
		{"evidence at 4 KiB", base64.StdEncoding.EncodeToString(ridA), a},
		{"partition at 4 KiB", pA, a[3*4096-10 : 3*4096+90]},
		{"evidence at 8 KiB", base64.StdEncoding.EncodeToString(ridB), b},
	} {
		got, _, err := restoreBytes(t, db.DB, c.hash, RestoreOptions{})
		if err != nil {
			t.Fatalf("%s: restore: %v", c.name, err)
		}
		if !bytes.Equal(got, c.want) {
			t.Fatalf("%s: restored %d bytes that differ from the %d stored", c.name, len(got), len(c.want))
		}
	}
}

// chunkBlobPath returns the blob file of the chunk at offset off of the
// evidence rid.
func chunkBlobPath(t *testing.T, db *badger.DB, rid []byte, off int64) string {
	t.Helper()
	chash, err := dbio.GetNode(util.AppendToBytesSlice(cnst.RelationNamespace, rid, cnst.DataSeperator, off), db)
	if err != nil {
		t.Fatalf("relation at %d: %v", off, err)
	}
	loc, err := dbio.LocateChunk(util.AppendToBytesSlice(cnst.ChonkNamespace, chash), db)
	if err != nil {
		t.Fatalf("locate chunk at %d: %v", off, err)
	}
	return dbio.ResolveBlobPath(db.Opts().Dir, loc.Path)
}

// swapChunkBlobs exchanges the stored bytes of the chunks at offsets x and y.
func swapChunkBlobs(t *testing.T, db *badger.DB, rid []byte, x, y int64) {
	t.Helper()
	px, py := chunkBlobPath(t, db, rid, x), chunkBlobPath(t, db, rid, y)
	bx, err := os.ReadFile(px)
	if err != nil {
		t.Fatal(err)
	}
	by, err := os.ReadFile(py)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(px, by, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(py, bx, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRestoreRejectsSubstitutedChunk is the RST-3 regression for chunks.
// Restore never checked what it read against the chunk hash. Legacy blobs
// carry nothing that ties them to their chunk (raw, or sealed without
// additional data), so a blob holding another chunk's bytes restored as
// wrong content without an error.
func TestRestoreRejectsSubstitutedChunk(t *testing.T) {
	for _, m := range fileModes {
		t.Run(m.Name, func(t *testing.T) {
			db := testdb.Open(t, m)
			util.SetChonkSize(4)
			content := testutil.RandomBytes(3, 3*4096)
			rid := seedCompletedEvidence(t, db.DB, "substituted", content, "")
			swapChunkBlobs(t, db.DB, rid, 0, 4096)
			rhash := base64.StdEncoding.EncodeToString(rid)

			_, _, err := restoreBytes(t, db.DB, rhash, RestoreOptions{})
			var corrupt *dbio.ChunkCorruptError
			if !errors.As(err, &corrupt) {
				t.Fatalf("restore of substituted chunks: err = %v, want a ChunkCorruptError", err)
			}
			if !errors.Is(err, blobcodec.ErrCorrupt) {
				t.Fatalf("ChunkCorruptError does not match blobcodec.ErrCorrupt: %v", err)
			}
			if corrupt.Offset != 0 && corrupt.Offset != 4096 {
				t.Fatalf("corrupt chunk reported at offset %d, want 0 or 4096", corrupt.Offset)
			}
			if !bytes.Equal(corrupt.EvidenceID, rid) || corrupt.Path == "" || len(corrupt.Hash) == 0 {
				t.Fatalf("ChunkCorruptError lacks its evidence, path or hash: %+v", corrupt)
			}

			// Verification can be turned off explicitly; the wrong bytes
			// then come back as stored.
			got, report, err := restoreBytes(t, db.DB, rhash, RestoreOptions{Verify: VerifyOff})
			if err != nil {
				t.Fatalf("restore with verification off: %v", err)
			}
			if bytes.Equal(got, content) || report.Digest != DigestSkipped {
				t.Fatalf("restore with verification off: content intact=%v digest=%q", bytes.Equal(got, content), report.Digest)
			}

			l, err := dbio.OpenLogical(db.DB, util.AppendToBytesSlice(cnst.EviFileNamespace, rid), dbio.ReadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := l.Stream(t.Context(), func([]byte) error { return nil }); !errors.As(err, &corrupt) {
				t.Fatalf("streaming substituted chunks: err = %v, want a ChunkCorruptError", err)
			}
		})
	}
}

// TestRestoreVerifiesRecordedHash is the RST-3 regression for whole objects:
// with every chunk intact, restore still compares what it wrote with the
// hash the record carries, in each form ingest computes it.
func TestRestoreVerifiesRecordedHash(t *testing.T) {
	for _, algo := range []string{cnst.BLAKE3, cnst.SHA3} {
		t.Run(algo, func(t *testing.T) {
			db := testdb.Open(t, testdb.Default)
			if err := cnst.SetHashAlgo(algo); err != nil {
				t.Fatal(err)
			}
			util.SetChonkSize(4)
			cs := int64(4096)
			content := testutil.RandomBytes(4, int(4*cs+123))
			other := testutil.RandomBytes(5, 10)

			cases := []struct {
				name     string
				fileHash string
				want     DigestStatus
				wantErr  error
			}{
				{"plain", plainHash(content), DigestVerified, nil},
				{"hocho", evidenceHocho(content, cs), DigestVerified, nil},
				{"none recorded", "", DigestUnavailable, nil},
				{"wrong", plainHash(other), "", ErrDigestMismatch},
			}
			for i, c := range cases {
				rid := seedCompletedEvidence(t, db.DB, "verify-"+string(rune('a'+i)), content, c.fileHash)
				got, report, err := restoreBytes(t, db.DB, base64.StdEncoding.EncodeToString(rid), RestoreOptions{})
				if c.wantErr != nil {
					if !errors.Is(err, c.wantErr) {
						t.Fatalf("%s: err = %v, want %v", c.name, err, c.wantErr)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s: restore: %v", c.name, err)
				}
				if !bytes.Equal(got, content) {
					t.Fatalf("%s: content differs", c.name)
				}
				if report.Digest != c.want || report.Size != int64(len(content)) || report.Chunks != 5 {
					t.Fatalf("%s: report %+v, want digest %q, size %d, 5 chunks", c.name, report, c.want, len(content))
				}
			}

			// A partition's hash is plain, or hocho over the chunks it covers
			// whole and the hash of each partly covered piece.
			rid := seedCompletedEvidence(t, db.DB, "verify-partitions", content, "")
			start, size := cs-100, 2*cs+300
			edges, err := legacyHochoWithEdges(rid, start, size, content, db.DB)
			if err != nil {
				t.Fatalf("hocho with edges: %v", err)
			}
			for _, p := range []struct{ name, fileHash string }{
				{"p-plain", plainHash(content[start : start+size])},
				{"p-hocho", base64.StdEncoding.EncodeToString(edges)},
			} {
				rhash := seedPartition(t, db.DB, rid, p.name, start, size, p.fileHash)
				got, report, err := restoreBytes(t, db.DB, rhash, RestoreOptions{})
				if err != nil {
					t.Fatalf("%s: restore: %v", p.name, err)
				}
				if !bytes.Equal(got, content[start:start+size]) || report.Digest != DigestVerified {
					t.Fatalf("%s: content intact=%v digest=%q", p.name, bytes.Equal(got, content[start:start+size]), report.Digest)
				}
			}

			// Chunk-level verification skips the whole-object hash.
			rhash := base64.StdEncoding.EncodeToString(seedCompletedEvidence(t, db.DB, "verify-chunks-only", content, plainHash(other)))
			if _, report, err := restoreBytes(t, db.DB, rhash, RestoreOptions{Verify: VerifyChunks}); err != nil || report.Digest != DigestSkipped {
				t.Fatalf("chunk-level restore: digest=%q err=%v", report.Digest, err)
			}
		})
	}
}

// TestLogicalReadAt reads evidence and partitions at arbitrary offsets
// through the logical reader.
func TestLogicalReadAt(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	util.SetChonkSize(4)
	cs := int64(4096)
	content := testutil.RandomBytes(6, int(5*cs+77))
	rid := seedCompletedEvidence(t, db.DB, "read-at", content, "")
	praw := seedPartition(t, db.DB, rid, "p", cs+5, 3*cs, "")
	pkey, err := base64.StdEncoding.DecodeString(praw)
	if err != nil {
		t.Fatal(err)
	}

	for _, obj := range []struct {
		name string
		fid  []byte
		want []byte
	}{
		{"evidence", util.AppendToBytesSlice(cnst.EviFileNamespace, rid), content},
		{"partition", util.AppendToBytesSlice(cnst.PartiFileNamespace, pkey), content[cs+5 : 4*cs+5]},
	} {
		l, err := dbio.OpenLogical(db.DB, obj.fid, dbio.ReadOptions{})
		if err != nil {
			t.Fatalf("%s: open: %v", obj.name, err)
		}
		if l.Size() != int64(len(obj.want)) {
			t.Fatalf("%s: size %d, want %d", obj.name, l.Size(), len(obj.want))
		}
		n := int64(len(obj.want))
		for _, r := range [][2]int64{{0, 1}, {0, n}, {cs - 1, 2}, {cs, cs}, {7, n - 8}, {n - 1, 1}, {n - 10, 10}} {
			buf := make([]byte, r[1])
			got, err := l.ReadAt(buf, r[0])
			if err != nil || int64(got) != r[1] || !bytes.Equal(buf, obj.want[r[0]:r[0]+r[1]]) {
				t.Fatalf("%s: ReadAt(%d, %d) = %d, %v; content matches %v", obj.name, r[1], r[0], got, err, bytes.Equal(buf[:got], obj.want[r[0]:r[0]+int64(got)]))
			}
		}
		buf := make([]byte, 20)
		got, err := l.ReadAt(buf, n-5)
		if got != 5 || err != io.EOF || !bytes.Equal(buf[:5], obj.want[n-5:]) {
			t.Fatalf("%s: ReadAt past the end = %d, %v, want 5, EOF", obj.name, got, err)
		}
		if _, err := l.ReadAt(buf, n); err != io.EOF {
			t.Fatalf("%s: ReadAt at the end: %v, want EOF", obj.name, err)
		}

		var streamed bytes.Buffer
		if err := l.Stream(context.Background(), func(p []byte) error { streamed.Write(p); return nil }); err != nil {
			t.Fatalf("%s: stream: %v", obj.name, err)
		}
		if !bytes.Equal(streamed.Bytes(), obj.want) {
			t.Fatalf("%s: streamed content differs", obj.name)
		}
	}
}
