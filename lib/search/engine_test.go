package search

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"
	"unicode/utf8"

	"indicer/internal/testutil/fsfixture"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/ingest"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// openSearchDB opens a database in 4 KiB chunks, so a few kilobytes of
// evidence cross several chunk boundaries.
func openSearchDB(t *testing.T) *badger.DB {
	t.Helper()
	d := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: 4})
	if err := util.EnsureBlobPath(d.Root); err != nil {
		t.Fatal(err)
	}
	return d.DB
}

// storeRaw stores content as the evidence eid, in the state given.
func storeRaw(t *testing.T, db *badger.DB, eid, content []byte, state structs.IngestState) {
	t.Helper()
	if err := dbio.CreateEvidence(db, eid, structs.NewEvidenceFile("ev.img", 0, int64(len(content)), nil, cnst.UnknownEvidenceType, "")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.IngestChunks(t.Context(), store.ChunkPass{DB: db, ID: eid, Source: bytes.NewReader(content), Size: int64(len(content)), Quiet: true}); err != nil {
		t.Fatal(err)
	}
	err := dbio.UpdateEvidence(db, eid, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		e.IngestState = state
		e.Completed = state == structs.IngestStateCompleted
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// putLegacyIndex records, in the form ingests wrote before W6, one partition
// covering the evidence and an indexed file for each range of files: names
// that carry the evidence and partition IDs, and a single range each.
func putLegacyIndex(t *testing.T, db *badger.DB, eid []byte, size int64, files map[string][2]int64) {
	t.Helper()
	pid := append([]byte("partition-"), eid[:6]...)
	members := map[string]structs.InternalOffset{}
	err := db.Update(func(txn *badger.Txn) error {
		for path, r := range files {
			iid := append([]byte("indexed-"+path+"-"), eid[:6]...)
			name := string(eid) + cnst.DataSeperator + base64.StdEncoding.EncodeToString(pid) + cnst.DataSeperator + path
			rec := structs.NewIndexedFile(name, r[0], r[1]-r[0], cnst.UnknownEvidenceType, false, "")
			if err := dbio.PutRecordTxn(db, txn, dbio.CanonicalIndexedKey(iid), rec); err != nil {
				return err
			}
			members[base64.StdEncoding.EncodeToString(iid)] = structs.InternalOffset{Start: r[0], End: r[1] - 1}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	pname := base64.StdEncoding.EncodeToString(eid) + cnst.DataSeperator + "ev.img_p0"
	if err := dbio.SetFile(dbio.CanonicalPartitionKey(pid), structs.NewPartitionFile(pname, 0, size, members, ""), db); err != nil {
		t.Fatal(err)
	}
	err = dbio.UpdateEvidence(db, eid, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		e.InternalObjects = map[string]structs.InternalOffset{base64.StdEncoding.EncodeToString(pid): {Start: 0, End: size - 1}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// foldCount counts the non-overlapping occurrences of query in data,
// folding ASCII letters only, as the search does for an ASCII query.
func foldCount(query string, data []byte) int64 {
	fold := func(b []byte) []byte {
		out := make([]byte, len(b))
		for i, c := range b {
			out[i] = toLowerASCII(c)
		}
		return out
	}
	return int64(bytes.Count(fold(data), fold([]byte(query))))
}

func summaryKey(key []byte) string { return base64.StdEncoding.EncodeToString(key) }

// plantedEvidence is five 4 KiB chunks and 100 bytes, with "needle" in
// assorted case at offsets chosen to test the chunk edges: inside a chunk,
// across a boundary, starting at a chunk's last byte, and ending at the end.
func plantedEvidence() []byte {
	b := bytes.Repeat([]byte("."), 5*4096+100)
	for off, s := range map[int]string{150: "needle", 1000: "NEEDLE", 4093: "Needle", 8191: "needle", 12280: "neEDle", 12286: "needle", len(b) - 6: "needle"} {
		copy(b[off:], s)
	}
	return b
}

// Files of the planted evidence: several share chunk 0 with different
// ranges (SRC-1); H holds the needle that starts at a chunk's last byte, and
// G ends two bytes into a needle (SRC-2).
var plantedFiles = map[string][2]int64{
	"/a": {100, 200},
	"/b": {300, 4500},
	"/d": {0, 4096},
	"/e": {900, 1100},
	"/g": {12000, 12290},
	"/h": {8000, 8300},
}

func checkSummary(t *testing.T, db *badger.DB, query string, want map[string]int64, wantTotal int64) {
	t.Helper()
	got, total, err := SearchSummaryWithContext(t.Context(), query, db)
	if err != nil {
		t.Fatal(err)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%q: %s has %d hits, want %d", query, k, got[k], n)
		}
	}
	for k, n := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%q: %s has %d hits, want none", query, k, n)
		}
	}
	if total != wantTotal {
		t.Errorf("%q: total %d, want %d", query, total, wantTotal)
	}
}

// expectedCounts is what the search must report for query over the planted
// evidence and its legacy index: each object's own count, objects without a
// hit left out.
func expectedCounts(query string, eid, content []byte) map[string]int64 {
	want := map[string]int64{}
	add := func(key []byte, data []byte) {
		if n := foldCount(query, data); n > 0 {
			want[summaryKey(key)] = n
		}
	}
	add(dbio.CanonicalEvidenceKey(eid), content)
	add(dbio.CanonicalPartitionKey(append([]byte("partition-"), eid[:6]...)), content)
	for path, r := range plantedFiles {
		add(dbio.CanonicalIndexedKey(append([]byte("indexed-"+path+"-"), eid[:6]...)), content[r[0]:r[1]])
	}
	return want
}

// TestSearchCountsAcrossChunkBoundaries (SRC-1, SRC-2, SRC-9): every object
// gets the hits in its own bytes, a match across a chunk boundary is counted
// once, nothing past an object's end is matched, and the total counts the
// stored bytes once rather than once per level.
func TestSearchCountsAcrossChunkBoundaries(t *testing.T) {
	t.Parallel()
	db := openSearchDB(t)
	eid := []byte("search-evidence1")
	content := plantedEvidence()
	storeRaw(t, db, eid, content, structs.IngestStateCompleted)
	putLegacyIndex(t, db, eid, int64(len(content)), plantedFiles)

	want := expectedCounts("needle", eid, content)
	if want[summaryKey(dbio.CanonicalEvidenceKey(eid))] != 7 {
		t.Fatalf("the planted evidence holds %d needles, want 7", want[summaryKey(dbio.CanonicalEvidenceKey(eid))])
	}
	checkSummary(t, db, "needle", want, 7)
}

// TestSearchLongQueryAcrossChunks (SRC-2): a query longer than the part of
// an object in its first chunk neither panics nor misses a match that
// crosses the boundary.
func TestSearchLongQueryAcrossChunks(t *testing.T) {
	t.Parallel()
	db := openSearchDB(t)
	eid := []byte("search-evidence2")
	content := bytes.Repeat([]byte("."), 3*4096)
	const long = "the-quick-brown-fox-jumps"
	copy(content[4080:], long)
	storeRaw(t, db, eid, content, structs.IngestStateCompleted)
	files := map[string][2]int64{"/f": {4090, 4200}, "/j": {4000, 4200}}
	putLegacyIndex(t, db, eid, int64(len(content)), files)

	want := map[string]int64{
		summaryKey(dbio.CanonicalEvidenceKey(eid)):                                       1,
		summaryKey(dbio.CanonicalPartitionKey(append([]byte("partition-"), eid[:6]...))): 1,
		summaryKey(dbio.CanonicalIndexedKey(append([]byte("indexed-/j-"), eid[:6]...))):  1,
	}
	checkSummary(t, db, long, want, 1)
}

// TestSearchSkipsUnstoredEvidenceAndQuarantinedIndexes: evidence that is
// not stored is not searched at all, and the partitions and files of a
// quarantined index are not searched, though its evidence is.
func TestSearchSkipsUnstoredEvidenceAndQuarantinedIndexes(t *testing.T) {
	t.Parallel()
	db := openSearchDB(t)
	content := plantedEvidence()
	stored, flushed, quarantined := []byte("search-evidence3"), []byte("search-evidence4"), []byte("search-evidence5")
	for _, eid := range [][]byte{stored, flushed, quarantined} {
		state := structs.IngestStateCompleted
		if bytes.Equal(eid, flushed) {
			state = structs.IngestStateFlushed
		}
		storeRaw(t, db, eid, content, state)
		putLegacyIndex(t, db, eid, int64(len(content)), plantedFiles)
	}
	err := dbio.UpdateEvidence(db, quarantined, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		e.Index = &structs.IndexInfo{State: structs.IndexInvalid, Reason: "quarantined"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	want := expectedCounts("needle", stored, content)
	want[summaryKey(dbio.CanonicalEvidenceKey(quarantined))] = 7
	checkSummary(t, db, "needle", want, 14)
}

// TestSearchCountsIndexedContent: on evidence indexed by the W6 index pass
// (extents with holes, inline, materialised content), every evidence,
// partition and file reports the hits in the bytes its reader serves.
func TestSearchCountsIndexedContent(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"ntfs/ntfs_compressed", "ext/ext4", "fat/fat16", "xfs/xfs"} {
		t.Run(name, func(t *testing.T) {
			db := openSearchDB(t)
			var path string
			for _, m := range fsfixture.All(t) {
				if m.Name == name {
					path = fsfixture.Open(t, m)
				}
			}
			if path == "" {
				t.Fatalf("no fixture %s", name)
			}
			res, err := ingest.Run(t.Context(), db, ingest.Request{Path: path, Decode: true, Quiet: true})
			if err != nil {
				t.Fatal(err)
			}
			objects := map[string][]byte{}
			read := func(key []byte) {
				l, err := dbio.OpenLogical(db, key, dbio.ReadOptions{})
				if errors.Is(err, dbio.ErrContentUnavailable) {
					return
				}
				if err != nil {
					t.Fatalf("%q: %v", key, err)
				}
				var data []byte
				if err := l.Stream(t.Context(), func(b []byte) error { data = append(data, b...); return nil }); err != nil {
					t.Fatalf("%q: %v", key, err)
				}
				objects[string(key)] = data
			}
			eid := res.EvidenceID
			read(dbio.CanonicalEvidenceKey(eid))
			e, err := dbio.GetEvidenceFile(eid, db)
			if err != nil {
				t.Fatal(err)
			}
			for pidB64 := range e.InternalObjects {
				pid, _ := base64.StdEncoding.DecodeString(pidB64)
				read(dbio.CanonicalPartitionKey(pid))
				prec, err := dbio.GetPartitionFile(pid, db)
				if err != nil {
					t.Fatal(err)
				}
				err = dbio.ForEachPartitionMember(db, pid, prec, func(iidB64 string, _ structs.InternalOffset) error {
					iid, _ := base64.StdEncoding.DecodeString(iidB64)
					read(dbio.CanonicalIndexedKey(iid))
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}

			// Queries taken from the content of files, so they hit; foldCount
			// is the reference for terms matched byte by byte (ASCII, or not
			// UTF-8).
			var queries []string
			for key, data := range objects {
				q := string(data[len(data)/2 : len(data)/2+min(5, len(data)/2)])
				if bytes.HasPrefix([]byte(key), []byte(cnst.IdxFileNamespace)) && len(data) > 64 && len(queries) < 3 &&
					(isASCII([]byte(q)) || !utf8.ValidString(q)) {
					queries = append(queries, q)
				}
			}
			queries = append(queries, "\x00\x00\x00")
			for _, q := range queries {
				want := map[string]int64{}
				for key, data := range objects {
					if n := foldCount(q, data); n > 0 {
						want[summaryKey([]byte(key))] = n
					}
				}
				checkSummary(t, db, q, want, foldCount(q, objects[string(dbio.CanonicalEvidenceKey(eid))]))
			}
		})
	}
}
