package index

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"testing"

	"indicer/internal/testutil/fsfixture"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/entropy"
	"indicer/lib/evidence"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// knownIndexDefects lists ground-truth entries the index pass cannot get
// right today, keyed "<fixture>|<path>", with the bug inventory ID.
var knownIndexDefects = map[string]string{
	"ext/ext4_fsagen|deleted:/old/tobedeleted.bin": "PAR-6 (ext4 zeroes a deleted inode's size and extent tree; residual)",
}

// expectedKinds names the content kind some ground-truth files must be
// indexed with, so the fixture test proves each path is taken.
var expectedKinds = map[string]string{
	"ntfs/ntfs_compressed|/z/text.txt":   structs.ContentMaterialized,
	"ntfs/ntfs_compressed|/z/random.bin": structs.ContentMaterialized,
	"ntfs/ntfs_compressed|/z/mixed.bin":  structs.ContentMaterialized,
	"ntfs/ntfs_compressed|/sparse.bin":   structs.ContentExtents,
	"ext/ext4|/inline.txt":               structs.ContentInline,
	"ext/ext4|/sparse.bin":               structs.ContentExtents,
	"ext/ext4|/empty.txt":                structs.ContentNone,
	"xfs/xfs|/multi.bin":                 structs.ContentExtents,
}

// recordsByPartition reads the evidence's indexed records grouped by their
// partition's offset, then path.
func recordsByPartition(t *testing.T, db *badger.DB, eid []byte) map[int64]map[string][]indexed {
	t.Helper()
	e, err := dbio.GetEvidenceFile(eid, db)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]map[string][]indexed{}
	for pidB64 := range e.InternalObjects {
		pid, _ := base64.StdEncoding.DecodeString(pidB64)
		prec, err := dbio.GetPartitionFile(pid, db)
		if err != nil {
			t.Fatalf("partition %s: %v", pidB64, err)
		}
		if out[prec.Start] == nil {
			out[prec.Start] = map[string][]indexed{}
		}
		err = dbio.ForEachPartitionMember(db, pid, prec, func(iidB64 string, _ structs.InternalOffset) error {
			iid, _ := base64.StdEncoding.DecodeString(iidB64)
			rec, err := dbio.GetIndexedFile(iid, db)
			if err != nil {
				return fmt.Errorf("indexed %s: %w", iidB64, err)
			}
			if !slices.Equal(rec.PartitionID, pid) {
				return fmt.Errorf("%s: partition %x, listed under %x", rec.Path, rec.PartitionID, pid)
			}
			out[prec.Start][rec.Path] = append(out[prec.Start][rec.Path], indexed{id: iid, rec: rec})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// indexKeys lists every key the index pass writes.
func indexKeys(t *testing.T, db *badger.DB) []string {
	t.Helper()
	var keys []string
	for _, ns := range []string{cnst.PartiFileNamespace, cnst.IdxFileNamespace, cnst.PartitionMemberNamespace,
		cnst.ObjectNamespace, cnst.ExtentPageNamespace, cnst.PartitionReportNamespace, cnst.HashLookupMemberNamespace, cnst.RelationNamespace} {
		err := db.View(func(txn *badger.Txn) error {
			opts := badger.DefaultIteratorOptions
			opts.Prefix = []byte(ns)
			opts.PrefetchValues = false
			it := txn.NewIterator(opts)
			defer it.Close()
			for it.Rewind(); it.Valid(); it.Next() {
				keys = append(keys, string(it.Item().KeyCopy(nil)))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// TestRunIndexesFixtures indexes every fixture image and checks that each
// file in its ground truth (live files, streams, and deleted files marked
// recoverable) has a record whose content is the true content, and that a
// second pass writes exactly the same keys (I7).
func TestRunIndexesFixtures(t *testing.T) {
	for _, m := range fsfixture.All(t) {
		t.Run(m.Name, func(t *testing.T) {
			src, err := evidence.Open(t.Context(), fsfixture.Open(t, m), evidence.Options{Decode: true})
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			img := src.Image()
			db := openIndexDB(t, img.Size())
			opts := Options{DB: db, EvidenceID: testEID, EvidenceName: "ev", Image: img, Entropy: true}
			res, err := Run(t.Context(), opts)
			if err != nil {
				t.Fatalf("index: %v", err)
			}
			if len(res.Partitions) != len(res.Report.Partitions) {
				t.Errorf("%d partition records for %d scanned partitions", len(res.Partitions), len(res.Report.Partitions))
			}
			if c := res.Coverage; c.FilesSeen+c.Symlinks != c.FilesIndexed {
				t.Errorf("coverage: %d files and %d symlinks seen, %d indexed", c.FilesSeen, c.Symlinks, c.FilesIndexed)
			}
			byPart := recordsByPartition(t, db, testEID)

			check := func(name string, recs []indexed, wantSize int64, wantSum string, deleted bool) {
				t.Helper()
				var tried []string
				for _, r := range recs {
					if r.rec.IsDeleted != deleted || r.rec.NodeKind != "file" {
						continue
					}
					if r.rec.Content.Kind == structs.ContentUnavailable {
						tried = append(tried, "unavailable")
						continue
					}
					b := contentOf(t, db, img, r.id, r.rec)
					if int64(len(b)) == wantSize && sha256Hex(b) == wantSum {
						if r.rec.FileHash != fileHash(db, b) {
							t.Errorf("%s: the record's hash is not the plain hash of its content", name)
						}
						if r.rec.Entropy == nil {
							t.Errorf("%s: no entropy recorded", name)
						} else if want := entropy.Of(b); math.Abs(*r.rec.Entropy-want) > 1e-9 {
							t.Errorf("%s: entropy %v, the content's is %v", name, *r.rec.Entropy, want)
						}
						if k, ok := expectedKinds[m.Name+"|"+name]; ok && r.rec.Content.Kind != k {
							t.Errorf("%s: indexed as %s, want %s", name, r.rec.Content.Kind, k)
						}
						return
					}
					tried = append(tried, fmt.Sprintf("%s %d bytes", r.rec.Content.Kind, len(b)))
				}
				t.Errorf("%s: no record with the true content (%d bytes); records: %v", name, wantSize, tried)
			}
			for _, part := range m.Partitions {
				recs := byPart[part.Offset]
				for _, f := range part.Files {
					if f.Kind != "file" {
						continue
					}
					check(f.Path, recs[f.Path], f.Size, f.SHA256, false)
					for _, s := range f.Streams {
						check(f.Path+":"+s.Name, recs[f.Path+":"+s.Name], s.Size, s.SHA256, false)
					}
				}
				for _, d := range part.Deleted {
					if d.Recoverable != "full" {
						continue
					}
					if _, known := knownIndexDefects[m.Name+"|deleted:"+d.Path]; known {
						continue
					}
					// Deleted names may be mangled (FAT replaces the first
					// character), so any deleted record may hold the content.
					var all []indexed
					for _, rs := range recs {
						all = append(all, rs...)
					}
					check("deleted "+d.Path, all, d.Size, d.SHA256, true)
				}
			}

			// A second pass over the same evidence writes the same keys.
			first := indexKeys(t, db)
			res2, err := Run(t.Context(), opts)
			if err != nil {
				t.Fatalf("second pass: %v", err)
			}
			if second := indexKeys(t, db); !slices.Equal(first, second) {
				t.Errorf("a second pass changed the keys: %d before, %d after", len(first), len(second))
			}
			if res2.Coverage.FilesIndexed != res.Coverage.FilesIndexed {
				t.Errorf("second pass indexed %d files, first %d", res2.Coverage.FilesIndexed, res.Coverage.FilesIndexed)
			}
		})
	}
}
