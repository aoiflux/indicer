package ingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/evidence"
	"indicer/lib/parser/core"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
)

const (
	mergePackage      = "package-one"
	otherMergePackage = "package-two"
	mergeActor        = "examiner"
	// mergeChunk is the chunk size of the 64 KiB test databases.
	mergeChunk = 64 << 10
)

// The merge tests run serially, before the package's parallel tests. Each
// holds two or three databases, and badger commits a memtable and cache
// sketches for every one as it opens: beside the rest of the package they
// took 12 GB and ran Windows out of commit.

// openMergeDB opens a database of mode m with the options given.
func openMergeDB(t *testing.T, m testdb.Mode, o testdb.Options) *badger.DB {
	t.Helper()
	d := testdb.Open(t, m, o)
	if err := util.EnsureBlobPath(d.Root); err != nil {
		t.Fatal(err)
	}
	return d.DB
}

func mergeOrigin(pkg string) structs.MergeInfo {
	return structs.MergeInfo{Package: pkg, SourceActor: mergeActor, At: 1}
}

// storeIn stores the file at path into db as dues store does.
func storeIn(t *testing.T, db *badger.DB, req Request) Result {
	t.Helper()
	req.Decode, req.Quiet = true, true
	res, err := Run(t.Context(), db, req)
	if err != nil {
		t.Fatalf("store %s: %v", req.Path, err)
	}
	return res
}

// mergeInto merges the evidence id of source into target in a session of
// its own.
func mergeInto(t *testing.T, target, source *badger.DB, id []byte, pkg string) (Result, error) {
	t.Helper()
	return mergeWith(t, target, MergeRequest{Source: source, SourceID: id, Origin: mergeOrigin(pkg), Quiet: true})
}

func mergeWith(t *testing.T, target *badger.DB, req MergeRequest) (Result, error) {
	t.Helper()
	s := NewSession(target)
	res, err := s.Merge(t.Context(), req)
	return res, s.closeWith(err)
}

func mergePresentWith(t *testing.T, target *badger.DB, req MergeRequest) (Result, error) {
	t.Helper()
	s := NewSession(target)
	res, err := s.MergePresent(t.Context(), req)
	return res, s.closeWith(err)
}

func editEvidence(t *testing.T, db *badger.DB, id []byte, fn func(e *structs.EvidenceFile)) {
	t.Helper()
	err := dbio.UpdateEvidence(db, id, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		fn(e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func evidenceOf(t *testing.T, db *badger.DB, id []byte) structs.EvidenceFile {
	t.Helper()
	e, err := dbio.GetEvidenceFile(id, db)
	if err != nil {
		t.Fatalf("evidence %x: %v", id, err)
	}
	return e
}

// makeLegacy strips what ingests since the content model record, as a
// record written before it lacks.
func makeLegacy(e *structs.EvidenceFile) {
	e.ContentModel, e.ChunkSize, e.ChunkHashAlgo, e.FileHashAlgo, e.HashStrategy = "", 0, "", "", ""
	e.PlainHash, e.ContentRoot, e.Features = "", "", nil
}

func relationKeyAt(id []byte, off int64) []byte {
	return util.AppendToBytesSlice(cnst.RelationNamespace, id, cnst.DataSeperator, off)
}

// swapRelations swaps the chunks two offsets of id name: every chunk still
// matches its hash, and the content is another.
func swapRelations(t *testing.T, db *badger.DB, id []byte, a, b int64) {
	t.Helper()
	err := db.Update(func(txn *badger.Txn) error {
		ka, kb := relationKeyAt(id, a), relationKeyAt(id, b)
		va, err := valueOf(txn, ka)
		if err != nil {
			return err
		}
		vb, err := valueOf(txn, kb)
		if err != nil {
			return err
		}
		if err := txn.Set(ka, vb); err != nil {
			return err
		}
		return txn.Set(kb, va)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func valueOf(txn *badger.Txn, key []byte) ([]byte, error) {
	item, err := txn.Get(key)
	if err != nil {
		return nil, err
	}
	return item.ValueCopy(nil)
}

func countKeys(t *testing.T, db *badger.DB, prefix []byte) int {
	t.Helper()
	n := 0
	err := db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.IteratorOptions{Prefix: prefix})
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func keyValues(t *testing.T, db *badger.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			v, err := it.Item().ValueCopy(nil)
			if err != nil {
				return err
			}
			out[string(it.Item().KeyCopy(nil))] = string(v)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func valueAt(t *testing.T, db *badger.DB, key []byte) ([]byte, bool) {
	t.Helper()
	var v []byte
	err := db.View(func(txn *badger.Txn) error {
		var err error
		v, err = valueOf(txn, key)
		return err
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return v, true
}

func aliasRecordAt(t *testing.T, db *badger.DB, key []byte) (structs.AliasRecord, bool) {
	t.Helper()
	var a structs.AliasRecord
	err := db.View(func(txn *badger.Txn) error { return dbio.GetRecordTxn(txn, key, &a) })
	if errors.Is(err, badger.ErrKeyNotFound) {
		return a, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return a, true
}

// mergedInfo is the MergeInfo the merge of the source evidence src records.
func mergedInfo(src structs.EvidenceFile, id []byte, pkg string) structs.MergeInfo {
	m := mergeOrigin(pkg)
	m.SourceEvidenceID, m.SourceFileHash, m.SourceHashAlgo = id, src.FileHash, src.FileHashAlgo
	return m
}

// TestMergeRoundTrip: an evidence merged into a database of another layout,
// encryption or hash strategy reads back as the same bytes, is stored in the
// target's layout, and keeps its name, type and source, with where it came
// from.
func TestMergeRoundTrip(t *testing.T) {
	hocho := testdb.Tuning()
	hocho.HashStrategy = cnst.HochoHashStrategy
	cases := []struct {
		name             string
		srcMode, dstMode testdb.Mode
		srcOpts, dstOpts testdb.Options
	}{
		{"256k-blake3-plain to 64k-sha3-encrypted", testdb.Default, testdb.Encrypted, testdb.Options{}, testdb.Options{ChunkKiB: 64, Hash: cnst.SHA3}},
		{"64k-sha3-encrypted to 256k-blake3-plain", testdb.Encrypted, testdb.Default, testdb.Options{ChunkKiB: 64, Hash: cnst.SHA3}, testdb.Options{}},
		{"hocho to sync", testdb.Default, testdb.Default, testdb.Options{ChunkKiB: 64, Tuning: &hocho}, testdb.Options{ChunkKiB: 64}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			source := openMergeDB(t, c.srcMode, c.srcOpts)
			target := openMergeDB(t, c.dstMode, c.dstOpts)
			stored := storeIn(t, source, Request{Path: fixture(t, "fat/fat16")})
			sid := stored.EvidenceID
			src := evidenceOf(t, source, sid)

			res, err := mergeInto(t, target, source, sid, mergePackage)
			if err != nil || res.Outcome != store.CommitStored || !bytes.Equal(res.EvidenceID, sid) {
				t.Fatalf("merge: %+v, %v", res, err)
			}
			want := sha256.Sum256(restoreBytes(t, source, sid))
			if got := sha256.Sum256(restoreBytes(t, target, res.EvidenceID)); got != want {
				t.Fatal("the merged evidence does not read back as the source")
			}
			e := evidenceOf(t, target, res.EvidenceID)
			chunk, file := hashNames(target)
			if e.ChunkSize != dbmeta.ChunkSize(target) || e.ChunkHashAlgo != chunk || e.FileHashAlgo != file || !e.Completed {
				t.Fatalf("merged layout: chunk %d %s, file %s, completed %v", e.ChunkSize, e.ChunkHashAlgo, e.FileHashAlgo, e.Completed)
			}
			if e.Name != src.Name || e.EvidenceType != src.EvidenceType || !reflect.DeepEqual(e.Source, src.Source) {
				t.Fatalf("merged %q %q %+v, source %q %q %+v", e.Name, e.EvidenceType, e.Source, src.Name, src.EvidenceType, src.Source)
			}
			m := e.Merged
			if m == nil || m.Package != mergePackage || m.SourceActor != mergeActor || !bytes.Equal(m.SourceEvidenceID, sid) ||
				m.SourceFileHash != src.FileHash || m.SourceHashAlgo != src.FileHashAlgo {
				t.Fatalf("merged info %+v", m)
			}
			if e.Index == nil || e.Index.State != structs.IndexComplete {
				t.Fatalf("index %+v", e.Index)
			}
		})
	}
}

// TestMergeRefusesSwappedRelation: a source whose relations name the right
// chunks at the wrong offsets reads without a corrupt chunk, but not as the
// content its record names; nothing is committed and the attempt stays
// resumable.
func TestMergeRefusesSwappedRelation(t *testing.T) {
	source := openDB(t)
	target := openDB(t)
	path := testutil.WriteRandomFile(t, t.TempDir(), "swap.bin", 21, 3*mergeChunk+100)
	sid := storeIn(t, source, Request{Path: path, NoIndex: true}).EvidenceID
	swapRelations(t, source, sid, 0, mergeChunk)

	res, err := mergeInto(t, target, source, sid, mergePackage)
	var mis *ContentMismatchError
	if !errors.As(err, &mis) || res.Outcome != "" {
		t.Fatalf("merge: %+v, %v; want a content mismatch", res, err)
	}
	if n := countKeys(t, target, []byte(cnst.EviMergeNamespace)); n != 0 {
		t.Fatalf("%d ledger keys after a mismatch", n)
	}
	for id, e := range evidenceRecords(t, target) {
		if e.Completed {
			t.Fatalf("evidence %x committed after a mismatch", id)
		}
	}
	if e := evidenceOf(t, target, res.Attempt); !e.Failed || e.IngestState != structs.IngestStatePending {
		t.Fatalf("attempt: failed %v, state %s", e.Failed, e.IngestState)
	}
}

// TestMergeChecksSecondAlgo: a source hashed with SHA-3 merges into a BLAKE3
// database, its content checked in its own algorithm; a recorded hash that
// is not the content's fails.
func TestMergeChecksSecondAlgo(t *testing.T) {
	source := openDBWith(t, testdb.Options{ChunkKiB: 64, Hash: cnst.SHA3})
	path := testutil.WriteRandomFile(t, t.TempDir(), "sha3.bin", 22, 2*mergeChunk+7)
	sid := storeIn(t, source, Request{Path: path, NoIndex: true}).EvidenceID
	if algo := evidenceOf(t, source, sid).FileHashAlgo; algo != dbmeta.FileHashSHA3 {
		t.Fatalf("source file hash %s", algo)
	}
	if res, err := mergeInto(t, openDB(t), source, sid, mergePackage); err != nil || res.Outcome != store.CommitStored {
		t.Fatalf("merge: %+v, %v", res, err)
	}

	editEvidence(t, source, sid, func(e *structs.EvidenceFile) {
		e.PlainHash = otherHash(e.PlainHash)
		e.FileHash = e.PlainHash
	})
	_, err := mergeInto(t, openDB(t), source, sid, mergePackage)
	var mis *ContentMismatchError
	if !errors.As(err, &mis) || mis.Algo != dbmeta.FileHashSHA3 {
		t.Fatalf("merge of a tampered hash: %v", err)
	}
}

// TestMergeLegacyHochoRootOtherLayout: a record from before the content
// model whose FileHash is its hocho root merges into the same layout and
// into another, where the root is computed in the source's layout.
func TestMergeLegacyHochoRootOtherLayout(t *testing.T) {
	hocho := testdb.Tuning()
	hocho.HashStrategy = cnst.HochoHashStrategy
	for name, dst := range map[string]testdb.Options{"same layout": {ChunkKiB: 64}, "other layout": {}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			source := openDBWith(t, testdb.Options{ChunkKiB: 64, Tuning: &hocho})
			path := testutil.WriteRandomFile(t, t.TempDir(), "legacy.bin", 23, 3*mergeChunk+5)
			sid := storeIn(t, source, Request{Path: path, NoIndex: true}).EvidenceID
			editEvidence(t, source, sid, makeLegacy)
			if e := evidenceOf(t, source, sid); e.FileHash == "" || e.ContentModel != "" {
				t.Fatalf("legacy record %+v", e)
			}
			target := openDBWith(t, dst)
			res, err := mergeInto(t, target, source, sid, mergePackage)
			if err != nil || res.Outcome != store.CommitStored {
				t.Fatalf("merge: %+v, %v", res, err)
			}
			content, _ := os.ReadFile(path)
			if !bytes.Equal(restoreBytes(t, target, res.EvidenceID), content) {
				t.Fatal("the merged evidence does not read back")
			}
			if algo := evidenceOf(t, target, res.EvidenceID).Merged.SourceHashAlgo; algo != dbmeta.FileHashBLAKE3 {
				t.Fatalf("recorded source algorithm %q", algo)
			}

			swapRelations(t, source, sid, 0, mergeChunk)
			_, err = mergeInto(t, openDBWith(t, dst), source, sid, mergePackage)
			var mis *ContentMismatchError
			if !errors.As(err, &mis) {
				t.Fatalf("merge of swapped legacy relations: %v", err)
			}
		})
	}
}

// TestMergeLegacyRootAtEvidenceChunkSize: a database whose layout was
// inferred keeps each evidence at the chunk size it was stored with, and a
// legacy hocho root is checked at that size, not the layout's, both when it
// is merged and when another package claims it.
func TestMergeLegacyRootAtEvidenceChunkSize(t *testing.T) {
	const layoutKiB = 1024
	hocho := testdb.Tuning()
	hocho.HashStrategy = cnst.HochoHashStrategy
	d := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: mergeChunk / int(cnst.KB), Tuning: &hocho})
	if err := util.EnsureBlobPath(d.Root); err != nil {
		t.Fatal(err)
	}
	source := d.DB
	path := testutil.WriteRandomFile(t, t.TempDir(), "stride.bin", 24, 3*mergeChunk+5)
	sid := storeIn(t, source, Request{Path: path, NoIndex: true}).EvidenceID
	editEvidence(t, source, sid, makeLegacy)
	d.Reconfigure(t, testdb.Options{ChunkKiB: layoutKiB, Tuning: &hocho})
	target := openDBWith(t, testdb.Options{})
	if dbmeta.ChunkSize(target) == mergeChunk || dbmeta.ChunkSize(source) == mergeChunk {
		t.Fatalf("source layout %d, target %d: neither may be the evidence's", dbmeta.ChunkSize(source), dbmeta.ChunkSize(target))
	}

	res, err := mergeInto(t, target, source, sid, mergePackage)
	if err != nil || res.Outcome != store.CommitStored {
		t.Fatalf("merge: %+v, %v", res, err)
	}
	content, _ := os.ReadFile(path)
	if !bytes.Equal(restoreBytes(t, target, res.EvidenceID), content) {
		t.Fatal("the merged evidence does not read back")
	}
	other := MergeRequest{Source: source, SourceID: sid, Origin: mergeOrigin(otherMergePackage), Quiet: true}
	if res, err := mergePresentWith(t, target, other); err != nil || res.Outcome != store.CommitPresent {
		t.Fatalf("present from another package: %+v, %v", res, err)
	}
}

// otherHash is a well-formed hash that names no content of the tests.
func otherHash(h string) string {
	sum := sha256.Sum256([]byte(h))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// TestMergeRefusesForgedRoot: a hocho record whose FileHash and ContentRoot
// name other content than its bytes, with an honest plain hash, is refused
// in its own layout and in another, and writes no ledger entry.
func TestMergeRefusesForgedRoot(t *testing.T) {
	for name, dst := range map[string]testdb.Options{"same layout": {ChunkKiB: mergeChunk / int(cnst.KB)}, "other layout": {}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			source := openDBStrategy(t, cnst.HochoHashStrategy)
			path := testutil.WriteRandomFile(t, t.TempDir(), "root.bin", 25, 3*mergeChunk+3)
			sid := storeIn(t, source, Request{Path: path, NoIndex: true}).EvidenceID
			editEvidence(t, source, sid, func(e *structs.EvidenceFile) {
				e.ContentRoot = otherHash(e.ContentRoot)
				e.FileHash = e.ContentRoot
			})
			target := openDBWith(t, dst)
			_, err := mergeInto(t, target, source, sid, mergePackage)
			var mis *ContentMismatchError
			if !errors.As(err, &mis) {
				t.Fatalf("merge of a forged root: %v", err)
			}
			if n := countKeys(t, target, []byte(cnst.EviMergeNamespace)); n != 0 {
				t.Fatalf("%d ledger keys after a forged root", n)
			}
		})
	}
}

// TestMergeRefusesForgedFileHash: a record whose plain hash and root are
// honest for its bytes but whose FileHash, which keys the merge ledger,
// names other content is refused: merged afresh, and claiming content
// another package merged under that FileHash.
func TestMergeRefusesForgedFileHash(t *testing.T) {
	for _, strategy := range []string{cnst.SyncHashStrategy, cnst.HochoHashStrategy} {
		t.Run(strategy, func(t *testing.T) {
			t.Parallel()
			source, target := openDBStrategy(t, strategy), openDB(t)
			path := testutil.WriteRandomFile(t, t.TempDir(), "forged.bin", 26, 3*mergeChunk+11)
			sid := storeIn(t, source, Request{Path: path, NoIndex: true}).EvidenceID
			if _, err := mergeInto(t, target, source, sid, mergePackage); err != nil {
				t.Fatal(err)
			}

			swapRelations(t, source, sid, 0, mergeChunk)
			swapped := restoreBytes(t, source, sid)
			sums, err := store.HashContent(t.Context(), source, bytes.NewReader(swapped), int64(len(swapped)), false)
			if err != nil {
				t.Fatal(err)
			}
			editEvidence(t, source, sid, func(e *structs.EvidenceFile) {
				e.PlainHash = base64.StdEncoding.EncodeToString(sums.Plain)
				e.ContentRoot = base64.StdEncoding.EncodeToString(sums.Root)
			})

			before := keyValues(t, target)
			other := MergeRequest{Source: source, SourceID: sid, Origin: mergeOrigin(otherMergePackage), Quiet: true}
			if _, err := mergePresentWith(t, target, other); !errors.Is(err, ErrNotMergeable) {
				t.Fatalf("present from another package with a forged file hash: %v, want %v", err, ErrNotMergeable)
			}
			if after := keyValues(t, target); !reflect.DeepEqual(before, after) {
				t.Fatal("aliases carried for a forged file hash")
			}
			fresh := openDB(t)
			if _, err := mergeInto(t, fresh, source, sid, mergePackage); !errors.Is(err, ErrNotMergeable) {
				t.Fatalf("merge of a forged file hash: %v, want %v", err, ErrNotMergeable)
			}
			if n := countKeys(t, fresh, []byte(cnst.EviMergeNamespace)); n != 0 {
				t.Fatalf("%d ledger keys after a forged file hash", n)
			}
		})
	}
}

// TestMergeLegacyRawContainer: a record from before the content model that
// stored an EWF set's raw bytes is merged as that container, not decoded and
// not indexed, whatever index it had, quarantined or not.
func TestMergeLegacyRawContainer(t *testing.T) {
	for name, idx := range map[string]structs.IndexInfo{
		"indexed":     {State: structs.IndexComplete},
		"quarantined": {State: structs.IndexInvalid, Reason: "quarantined"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			source := openDB(t)
			res, err := Run(t.Context(), source, Request{Path: fixture(t, "ewf/fat12_multiseg"), Quiet: true})
			if err != nil {
				t.Fatal(err)
			}
			sid := res.EvidenceID
			editEvidence(t, source, sid, func(e *structs.EvidenceFile) {
				makeLegacy(e)
				e.Source = &structs.EvidenceSource{Format: structs.SourceRaw, Members: e.Source.Members}
				e.Index = &idx
			})
			target := openDB(t)
			mres, err := mergeInto(t, target, source, sid, mergePackage)
			if err != nil {
				t.Fatal(err)
			}
			e := evidenceOf(t, target, mres.EvidenceID)
			if e.Source.Format != structs.SourceRaw || e.Source.Container != string(core.FormatEWF) {
				t.Fatalf("source %+v", e.Source)
			}
			if e.Index == nil || e.Index.State != structs.IndexSkipped || e.Index.Reason != evidence.SkipNotDecoded {
				t.Fatalf("index %+v", e.Index)
			}
		})
	}
}

// TestMergeNilSource: a legacy record with no source merges as a raw source.
func TestMergeNilSource(t *testing.T) {
	source := openDB(t)
	path := testutil.WriteRandomFile(t, t.TempDir(), "nosource.bin", 24, mergeChunk+3)
	sid := storeIn(t, source, Request{Path: path, NoIndex: true}).EvidenceID
	editEvidence(t, source, sid, func(e *structs.EvidenceFile) {
		makeLegacy(e)
		e.Source = nil
	})
	target := openDB(t)
	res, err := mergeInto(t, target, source, sid, mergePackage)
	if err != nil || res.Outcome != store.CommitStored {
		t.Fatalf("merge: %+v, %v", res, err)
	}
	if e := evidenceOf(t, target, res.EvidenceID); e.Source == nil || !reflect.DeepEqual(*e.Source, structs.EvidenceSource{Format: structs.SourceRaw}) {
		t.Fatalf("source %+v", e.Source)
	}
}

// TestMergeLegacyName: a legacy name joined with the separator merges as its
// last part.
func TestMergeLegacyName(t *testing.T) {
	source := openDB(t)
	path := testutil.WriteRandomFile(t, t.TempDir(), "named.bin", 25, mergeChunk)
	sid := storeIn(t, source, Request{Path: path, NoIndex: true}).EvidenceID
	const clean = "disk.img"
	editEvidence(t, source, sid, func(e *structs.EvidenceFile) { e.Name = "case" + cnst.DataSeperator + clean })
	target := openDB(t)
	res, err := mergeInto(t, target, source, sid, mergePackage)
	if err != nil {
		t.Fatal(err)
	}
	if name := evidenceOf(t, target, res.EvidenceID).Name; name != clean {
		t.Fatalf("name %q, want %q", name, clean)
	}
}

// TestMergeKeepsSourceID: an ID no evidence of the target uses is kept, and
// the ledger maps the source evidence to it.
func TestMergeKeepsSourceID(t *testing.T) {
	source, target := openDB(t), openDB(t)
	path := testutil.WriteRandomFile(t, t.TempDir(), "keep.bin", 26, mergeChunk)
	sid := storeIn(t, source, Request{Path: path, NoIndex: true}).EvidenceID
	res, err := mergeInto(t, target, source, sid, mergePackage)
	if err != nil || !bytes.Equal(res.EvidenceID, sid) {
		t.Fatalf("merge: %+v, %v", res, err)
	}
	entry, present, err := store.MergedTarget(target, mergedInfo(evidenceOf(t, source, sid), sid, mergePackage), res.Size)
	if err != nil || !present || !bytes.Equal(entry.Target, sid) {
		t.Fatalf("ledger %+v, present %v, %v", entry, present, err)
	}
}

// TestMergeReassignsUsedID: an ID the target already uses for other content
// is not taken; the merged evidence gets a new one, which the ledger names.
func TestMergeReassignsUsedID(t *testing.T) {
	source, target := openDB(t), openDB(t)
	dir := t.TempDir()
	sid := storeIn(t, source, Request{Path: testutil.WriteRandomFile(t, dir, "src.bin", 27, mergeChunk), NoIndex: true}).EvidenceID
	local := storeIn(t, target, Request{Path: testutil.WriteRandomFile(t, dir, "local.bin", 28, 2*mergeChunk), NoIndex: true}).EvidenceID
	rec := evidenceOf(t, target, local)
	if err := dbio.UpdateTxn(target, func(txn *badger.Txn) error { return dbio.SetEvidenceTxn(target, txn, sid, rec) }); err != nil {
		t.Fatal(err)
	}
	res, err := mergeInto(t, target, source, sid, mergePackage)
	if err != nil || res.Outcome != store.CommitStored {
		t.Fatalf("merge: %+v, %v", res, err)
	}
	id, err := uuid.FromBytes(res.EvidenceID)
	if err != nil || bytes.Equal(res.EvidenceID, sid) || id.Version() != uuid.Version(7) {
		t.Fatalf("merged ID %x (source %x): %v", res.EvidenceID, sid, err)
	}
	entry, present, err := store.MergedTarget(target, mergedInfo(evidenceOf(t, source, sid), sid, mergePackage), res.Size)
	if err != nil || !present || !bytes.Equal(entry.Target, res.EvidenceID) {
		t.Fatalf("ledger %+v, present %v, %v", entry, present, err)
	}
}

// TestMergePresentOnRerun: merging the same evidence again finds it present
// and writes nothing.
func TestMergePresentOnRerun(t *testing.T) {
	source, target := openDB(t), openDB(t)
	sid := storeIn(t, source, Request{Path: fixture(t, "fat/fat12")}).EvidenceID
	first, err := mergeInto(t, target, source, sid, mergePackage)
	if err != nil {
		t.Fatal(err)
	}
	before := keyValues(t, target)
	again, err := mergeInto(t, target, source, sid, mergePackage)
	if err != nil || again.Outcome != store.CommitPresent || !bytes.Equal(again.EvidenceID, first.EvidenceID) {
		t.Fatalf("rerun: %+v, %v", again, err)
	}
	if after := keyValues(t, target); !reflect.DeepEqual(before, after) {
		t.Fatal("a merge of present evidence changed the database")
	}
}

// TestMergeAliasesHeldContent: content the target already stores is aliased,
// not stored again; the ledger names the evidence holding it, the merged
// alias records where it came from, and a local alias at the same path
// stays as it was.
func TestMergeAliasesHeldContent(t *testing.T) {
	source, target := openDB(t), openDB(t)
	dir := t.TempDir()
	first := testutil.WriteRandomFile(t, dir, "first.bin", 29, 2*mergeChunk+9)
	content, _ := os.ReadFile(first)
	shared := filepath.Join(dir, "shared.bin")
	if err := os.WriteFile(shared, content, 0o600); err != nil {
		t.Fatal(err)
	}
	winner := storeIn(t, target, Request{Path: first, NoIndex: true}).EvidenceID
	if r := storeIn(t, target, Request{Path: shared, NoIndex: true}); r.Outcome != store.CommitAliased {
		t.Fatalf("local copy: %+v", r)
	}
	sid := storeIn(t, source, Request{Path: shared, NoIndex: true}).EvidenceID
	src := evidenceOf(t, source, sid)
	localKey := dbio.AliasKey(winner, src.Source.Members[0].Path)
	local, ok := valueAt(t, target, localKey)
	if !ok {
		t.Fatal("no local alias at the shared path")
	}

	res, err := mergeInto(t, target, source, sid, mergePackage)
	if err != nil || res.Outcome != store.CommitAliased || !bytes.Equal(res.EvidenceID, winner) {
		t.Fatalf("merge: %+v, %v; want an alias of %x", res, err, winner)
	}
	m := mergedInfo(src, sid, mergePackage)
	entry, present, err := store.MergedTarget(target, m, src.Size)
	if err != nil || !present || !bytes.Equal(entry.Target, winner) {
		t.Fatalf("ledger %+v, present %v, %v", entry, present, err)
	}
	a, ok := aliasRecordAt(t, target, dbio.MergeAliasKey(winner, m, src.Size, src.Source.Members[0].Path))
	if !ok || a.Merged == nil || a.Merged.Package != mergePackage {
		t.Fatalf("merged alias %+v, found %v", a, ok)
	}
	if v, _ := valueAt(t, target, localKey); !bytes.Equal(v, local) {
		t.Fatal("the merge changed the local alias at the same path")
	}
}

// TestMergePresentOtherPackageVerifies: a package that claims content
// another package merged has its own bytes read and checked before its
// aliases are carried; the package that merged it is not read again.
func TestMergePresentOtherPackageVerifies(t *testing.T) {
	source, target := openDB(t), openDB(t)
	dir := t.TempDir()
	path := testutil.WriteRandomFile(t, dir, "claimed.bin", 30, 3*mergeChunk+1)
	content, _ := os.ReadFile(path)
	copyPath := filepath.Join(dir, "claimed-copy.bin")
	if err := os.WriteFile(copyPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sid := storeIn(t, source, Request{Path: path, NoIndex: true}).EvidenceID
	storeIn(t, source, Request{Path: copyPath, NoIndex: true})
	src := evidenceOf(t, source, sid)
	if _, err := mergeInto(t, target, source, sid, mergePackage); err != nil {
		t.Fatal(err)
	}
	if a, ok := aliasRecordAt(t, target, dbio.MergeAliasKey(sid, mergedInfo(src, sid, mergePackage), src.Size, copyPath)); !ok || a.Merged == nil || a.Merged.Package != mergePackage {
		t.Fatalf("the merge carried the source's alias as %+v, found %v", a, ok)
	}
	// A copy stored in the source later, which only the other package names.
	laterPath := filepath.Join(dir, "claimed-later.bin")
	if err := os.WriteFile(laterPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	storeIn(t, source, Request{Path: laterPath, NoIndex: true})
	other := MergeRequest{Source: source, SourceID: sid, Origin: mergeOrigin(otherMergePackage), Quiet: true}
	aliasKey := dbio.MergeAliasKey(sid, mergedInfo(src, sid, otherMergePackage), src.Size, laterPath)

	swapRelations(t, source, sid, 0, mergeChunk)
	_, err := mergePresentWith(t, target, other)
	var mis *ContentMismatchError
	if !errors.As(err, &mis) {
		t.Fatalf("present from another package with other bytes: %v", err)
	}
	if _, ok := aliasRecordAt(t, target, aliasKey); ok {
		t.Fatal("aliases carried for bytes that are not the content")
	}

	swapRelations(t, source, sid, 0, mergeChunk)
	res, err := mergePresentWith(t, target, other)
	if err != nil || res.Outcome != store.CommitPresent || !bytes.Equal(res.EvidenceID, sid) {
		t.Fatalf("present from another package: %+v, %v", res, err)
	}
	a, ok := aliasRecordAt(t, target, aliasKey)
	if !ok || a.Merged == nil || a.Merged.Package != otherMergePackage || len(a.AttemptID) != 0 {
		t.Fatalf("carried alias %+v, found %v", a, ok)
	}

	swapRelations(t, source, sid, 0, mergeChunk)
	if _, err := mergePresentWith(t, target, MergeRequest{Source: source, SourceID: sid, Origin: mergeOrigin(mergePackage), Quiet: true}); err != nil {
		t.Fatalf("present from the package that merged it: %v", err)
	}
}

// TestMergePresentSharedIDOtherPackageVerifies: merged evidence keeps the
// source's ID, so a package that copies its record and chunk hashes finds it
// by that ID, past the ledger. Its bytes are read before its aliases are
// carried, as for a ledger entry another package wrote.
func TestMergePresentSharedIDOtherPackageVerifies(t *testing.T) {
	source, target := openDB(t), openDB(t)
	dir := t.TempDir()
	path := testutil.WriteRandomFile(t, dir, "shared.bin", 31, 3*mergeChunk+1)
	content, _ := os.ReadFile(path)
	copyPath := filepath.Join(dir, "shared-copy.bin")
	if err := os.WriteFile(copyPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sid := storeIn(t, source, Request{Path: path, NoIndex: true}).EvidenceID
	storeIn(t, source, Request{Path: copyPath, NoIndex: true})
	if _, err := mergeInto(t, target, source, sid, mergePackage); err != nil {
		t.Fatal(err)
	}
	src := evidenceOf(t, source, sid)
	if src.ContentRoot == "" || src.FileHash == src.ContentRoot {
		t.Fatalf("want a source keyed by its plain hash that records its root: %+v", src)
	}
	// A FileHash naming the root keys another ledger entry than the merge
	// wrote, so only the shared ID finds the target.
	claim := func(root string) []byte {
		editEvidence(t, source, sid, func(e *structs.EvidenceFile) {
			e.HashStrategy, e.FileHash, e.ContentRoot = cnst.HochoHashStrategy, root, root
		})
		m := mergedInfo(evidenceOf(t, source, sid), sid, otherMergePackage)
		return dbio.MergeAliasKey(sid, m, src.Size, copyPath)
	}
	other := MergeRequest{Source: source, SourceID: sid, Origin: mergeOrigin(otherMergePackage), Quiet: true}

	aliasKey := claim(otherHash(src.ContentRoot))
	_, err := mergePresentWith(t, target, other)
	var mis *ContentMismatchError
	if !errors.As(err, &mis) {
		t.Fatalf("a shared ID claimed with a root its bytes do not have: %v", err)
	}
	if _, ok := aliasRecordAt(t, target, aliasKey); ok {
		t.Fatal("aliases carried for a root the bytes do not have")
	}

	aliasKey = claim(src.ContentRoot)
	res, err := mergePresentWith(t, target, other)
	if err != nil || res.Outcome != store.CommitPresent || !bytes.Equal(res.EvidenceID, sid) {
		t.Fatalf("a shared ID claimed with its root: %+v, %v", res, err)
	}
	if a, ok := aliasRecordAt(t, target, aliasKey); !ok || a.Merged == nil || a.Merged.Package != otherMergePackage {
		t.Fatalf("carried alias %+v, found %v", a, ok)
	}
}

// TestMergeKeepsEWFVerified: an EWF set's stored digests are verified again
// over the merged bytes: a verified source stays verified, and a stored
// digest that does not match is flagged, or refused when verification is
// required.
func TestMergeKeepsEWFVerified(t *testing.T) {
	source := openDB(t)
	sid := storeIn(t, source, Request{Path: fixture(t, "ewf/fat12_multiseg")}).EvidenceID
	target := openDB(t)
	res, err := mergeInto(t, target, source, sid, mergePackage)
	if err != nil {
		t.Fatal(err)
	}
	if e := evidenceOf(t, target, res.EvidenceID); e.Source.EWF == nil || e.Source.EWF.Result != structs.EWFVerified {
		t.Fatalf("EWF %+v", e.Source.EWF)
	}

	editEvidence(t, source, sid, func(e *structs.EvidenceFile) {
		md5 := slices.Clone(e.Source.EWF.StoredMD5)
		md5[0] ^= 0xff
		e.Source.EWF.StoredMD5 = md5
	})
	target = openDB(t)
	res, err = mergeInto(t, target, source, sid, mergePackage)
	var mis *DigestMismatchError
	if !errors.As(err, &mis) || !mis.Stored || res.Outcome != store.CommitStored {
		t.Fatalf("merge of an altered digest: %+v, %v", res, err)
	}
	if e := evidenceOf(t, target, res.EvidenceID); e.Source.EWF.Result != structs.EWFMismatch {
		t.Fatalf("EWF %+v", e.Source.EWF)
	}

	target = openDB(t)
	res, err = mergeWith(t, target, MergeRequest{Source: source, SourceID: sid, Origin: mergeOrigin(mergePackage), Quiet: true, RequireVerified: true})
	if !errors.As(err, &mis) || mis.Stored || res.Outcome != "" {
		t.Fatalf("merge requiring verification: %+v, %v", res, err)
	}
}

// TestMergeSkipReasons: evidence not indexed for its content, or on
// request, is merged with the same index state and reason; an index the
// source quarantined is built afresh.
func TestMergeSkipReasons(t *testing.T) {
	source, target := openDB(t), openDB(t)
	dir := t.TempDir()
	opaque := storeIn(t, source, Request{Path: testutil.WriteRandomFile(t, dir, "opaque.bin", 31, mergeChunk), NoIndex: true}).EvidenceID
	editEvidence(t, source, opaque, func(e *structs.EvidenceFile) {
		e.Source.Format, e.Source.Container = structs.SourceOpaque, "vmdk"
		e.Index = &structs.IndexInfo{State: structs.IndexSkipped, Reason: evidence.SkipUnsupportedContainer}
	})
	undecoded, err := Run(t.Context(), source, Request{Path: fixture(t, "ewf/fat12_multiseg"), Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.bin")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ids := map[string][]byte{
		"opaque":        opaque,
		"undecoded":     undecoded.EvidenceID,
		"empty":         storeIn(t, source, Request{Path: empty}).EvidenceID,
		"not requested": storeIn(t, source, Request{Path: testutil.WriteRandomFile(t, dir, "x.bin", 32, mergeChunk), NoIndex: true}).EvidenceID,
	}
	s := NewSession(target)
	t.Cleanup(func() { _ = s.Close() })
	for name, id := range ids {
		want := evidenceOf(t, source, id).Index
		res, err := s.Merge(t.Context(), MergeRequest{Source: source, SourceID: id, Origin: mergeOrigin(mergePackage), Quiet: true})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := evidenceOf(t, target, res.EvidenceID).Index
		if want == nil || got == nil || got.State != structs.IndexSkipped || got.State != want.State || got.Reason != want.Reason {
			t.Errorf("%s: index %+v, source %+v", name, got, want)
		}
	}

	invalid := storeIn(t, source, Request{Path: fixture(t, "fat/fat12")}).EvidenceID
	editEvidence(t, source, invalid, func(e *structs.EvidenceFile) {
		e.Index = &structs.IndexInfo{State: structs.IndexInvalid, Reason: "quarantined"}
	})
	res, err := s.Merge(t.Context(), MergeRequest{Source: source, SourceID: invalid, Origin: mergeOrigin(mergePackage), Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := evidenceOf(t, target, res.EvidenceID).Index; got == nil || got.State != structs.IndexComplete {
		t.Fatalf("quarantined source: index %+v", got)
	}
}

// TestMergeCarriesFeatures: the merged evidence is stored with the features
// its source was stored with.
func TestMergeCarriesFeatures(t *testing.T) {
	source, target := openDB(t), openDB(t)
	all := storeIn(t, source, Request{Path: fixture(t, "fat/fat12"), FTS: true, Enrich: true, RevRel: true, Simhash: true}).EvidenceID
	none := storeIn(t, source, Request{Path: fixture(t, "fat/fat16")}).EvidenceID
	s := NewSession(target)
	t.Cleanup(func() { _ = s.Close() })
	for _, id := range [][]byte{all, none} {
		res, err := s.Merge(t.Context(), MergeRequest{Source: source, SourceID: id, Origin: mergeOrigin(mergePackage), Quiet: true})
		if err != nil {
			t.Fatal(err)
		}
		want := slices.Sorted(slices.Values(evidenceOf(t, source, id).Features))
		got := slices.Sorted(slices.Values(evidenceOf(t, target, res.EvidenceID).Features))
		if !slices.Equal(got, want) {
			t.Errorf("features %v, source %v", got, want)
		}
	}
	if f := evidenceOf(t, source, all).Features; len(f) != 4 {
		t.Fatalf("source features %v", f)
	}
}

// TestMergeSessionOpensStoresOnce: a session that merges several evidence
// with the full-text and graph features opens each store once.
func TestMergeSessionOpensStoresOnce(t *testing.T) {
	source, target := openDB(t), openDB(t)
	var ids [][]byte
	for _, name := range []string{"fat/fat12", "fat/fat16", "part/mbr_two"} {
		ids = append(ids, storeIn(t, source, Request{Path: fixture(t, name), FTS: true, Enrich: true}).EvidenceID)
	}
	s := NewSession(target)
	for _, id := range ids {
		if _, err := s.Merge(t.Context(), MergeRequest{Source: source, SourceID: id, Origin: mergeOrigin(mergePackage), Quiet: true}); err != nil {
			t.Fatalf("merge %x: %v", id, err)
		}
	}
	if got, want := s.Opens(), (Opens{Text: 1, Graph: 1}); got != want {
		t.Fatalf("opens = %+v, want %+v", got, want)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestMergeRefusesNotMergeable: evidence that is not committed, a duplicate,
// a range of another, or without a content hash is not merged; a record
// whose state alone says it is committed is.
func TestMergeRefusesNotMergeable(t *testing.T) {
	source, target := openDB(t), openDB(t)
	dir := t.TempDir()
	cases := []struct {
		name  string
		edit  func(e *structs.EvidenceFile)
		hint  string
		merge bool
	}{
		{"pending", func(e *structs.EvidenceFile) { e.Completed, e.IngestState = false, structs.IngestStatePending }, "", false},
		{"flushed", func(e *structs.EvidenceFile) { e.Completed, e.IngestState = false, structs.IngestStateFlushed }, "dues repair --fix", false},
		{"duplicate", func(e *structs.EvidenceFile) { e.IngestState = structs.IngestStateDuplicate }, "", false},
		{"offset", func(e *structs.EvidenceFile) { e.Start = 1 }, "", false},
		{"no hash", func(e *structs.EvidenceFile) { e.FileHash = "" }, "", false},
		{"file hash of other content", func(e *structs.EvidenceFile) { e.FileHash = otherHash(e.FileHash) }, "file hash", false},
		{"state completed", func(e *structs.EvidenceFile) { e.Completed = false }, "", true},
	}
	for i, c := range cases {
		path := testutil.WriteRandomFile(t, dir, c.name+".bin", uint64(40+i), mergeChunk+i)
		id := storeIn(t, source, Request{Path: path, NoIndex: true}).EvidenceID
		editEvidence(t, source, id, c.edit)
		res, err := mergeInto(t, target, source, id, mergePackage)
		switch {
		case c.merge && (err != nil || res.Outcome != store.CommitStored):
			t.Errorf("%s: %+v, %v; want it merged", c.name, res, err)
		case !c.merge && !errors.Is(err, ErrNotMergeable):
			t.Errorf("%s: %v, want %v", c.name, err, ErrNotMergeable)
		case !c.merge && !strings.Contains(err.Error(), c.hint):
			t.Errorf("%s: %v does not say %q", c.name, err, c.hint)
		}
	}
}
