package store

import (
	"bytes"
	"crypto/sha3"
	"encoding/base64"
	"errors"
	"hash"
	"maps"
	"slices"
	"strconv"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/vmihailenco/msgpack/v5"
)

const (
	testPackage      = "package-one"
	otherTestPackage = "package-two"
	uuidV7           = uuid.Version(7)
)

// testOrigin is the MergeInfo of the source evidence src of a test package.
func testOrigin(pkg string, src []byte, content []byte) structs.MergeInfo {
	return structs.MergeInfo{
		Package:          pkg,
		SourceEvidenceID: src,
		SourceFileHash:   base64.StdEncoding.EncodeToString(sum(content)),
		SourceHashAlgo:   dbmeta.FileHashBLAKE3,
		At:               1,
	}
}

func newUUID(t *testing.T) []byte {
	t.Helper()
	id, err := uuid.NewRandom()
	if err != nil {
		t.Fatal(err)
	}
	return id[:]
}

// flushMerged begins the merge attempt of m holding content, from path, as
// the merge does, and leaves it FLUSHED.
func flushMerged(t *testing.T, db *badger.DB, m structs.MergeInfo, path string, content []byte) []byte {
	t.Helper()
	rec := attemptRecord(db, path, content)
	rec.Merged = &m
	eid, _, err := BeginAttemptID(db, dbio.MergeAttemptKey(m, rec.Size), m.SourceEvidenceID, rec)
	if err != nil {
		t.Fatal(err)
	}
	flushChunks(t, db, eid, content, false)
	return eid
}

func commitStored(t *testing.T, db *badger.DB, eid []byte) {
	t.Helper()
	res, err := CommitEvidence(db, eid)
	if err != nil || res.Outcome != CommitStored {
		t.Fatalf("commit %x: %+v, %v", eid, res, err)
	}
}

func ledgerEntry(t *testing.T, db *badger.DB, m structs.MergeInfo, size int64) structs.MergeLedgerEntry {
	t.Helper()
	var entry structs.MergeLedgerEntry
	err := db.View(func(txn *badger.Txn) error {
		return dbio.GetRecordTxn(txn, dbio.MergeLedgerKey(m, size), &entry)
	})
	if err != nil {
		t.Fatalf("ledger entry of %x: %v", m.SourceEvidenceID, err)
	}
	return entry
}

func aliasAt(t *testing.T, db *badger.DB, key []byte) structs.AliasRecord {
	t.Helper()
	var a structs.AliasRecord
	if err := db.View(func(txn *badger.Txn) error { return dbio.GetRecordTxn(txn, key, &a) }); err != nil {
		t.Fatalf("alias %q: %v", key, err)
	}
	return a
}

func evidenceRecord(t *testing.T, db *badger.DB, eid []byte) structs.EvidenceFile {
	t.Helper()
	e, err := dbio.GetEvidenceFile(eid, db)
	if err != nil {
		t.Fatalf("evidence %x: %v", eid, err)
	}
	return e
}

func keyExists(t *testing.T, db *badger.DB, key []byte) bool {
	t.Helper()
	found := false
	err := db.View(func(txn *badger.Txn) error {
		_, err := txn.Get(key)
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func allKeys(t *testing.T, db *badger.DB) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			v, err := it.Item().ValueCopy(nil)
			if err != nil {
				return err
			}
			out[string(it.Item().KeyCopy(nil))] = v
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func pendingRecord() structs.EvidenceFile {
	rec := structs.NewEvidenceFile("e", 0, 100, nil, cnst.UnknownEvidenceType, "")
	rec.ContentModel, rec.Fingerprint, rec.ChunkSize = structs.ContentModelV2, "fp", 4096
	return rec
}

func assertV7(t *testing.T, eid, preferred []byte) {
	t.Helper()
	id, err := uuid.FromBytes(eid)
	if err != nil || id.Version() != uuidV7 || bytes.Equal(eid, preferred) {
		t.Fatalf("ID %x (%v), want a new UUIDv7 other than %x", eid, err, preferred)
	}
}

func TestBeginAttemptIDKeepsUnusedID(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default)
	preferred := newUUID(t)
	key := dbio.AttemptKey("/case/e.dd")
	eid, resumed, err := BeginAttemptID(db.DB, key, preferred, pendingRecord())
	if err != nil || resumed || !bytes.Equal(eid, preferred) {
		t.Fatalf("BeginAttemptID = %x resumed %v, %v; want %x", eid, resumed, err, preferred)
	}
	if e := evidenceRecord(t, db.DB, preferred); e.Attempt != 1 || e.IngestState != structs.IngestStatePending {
		t.Fatalf("record %+v", e)
	}
	if v := allKeys(t, db.DB)[string(key)]; !bytes.Equal(v, preferred) {
		t.Fatalf("attempt key names %x", v)
	}
}

func TestBeginAttemptIDReassignsUsedID(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default)
	used := func(state structs.IngestState, completed, failed bool) func(t *testing.T, id []byte) {
		return func(t *testing.T, id []byte) {
			rec := pendingRecord()
			rec.IngestState, rec.Completed, rec.Failed = state, completed, failed
			if err := dbio.CreateEvidence(db.DB, id, rec); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := []struct {
		name string
		use  func(t *testing.T, id []byte)
	}{
		{"completed", used(structs.IngestStateCompleted, true, false)},
		{"pending", used(structs.IngestStatePending, false, false)},
		{"failed", used(structs.IngestStatePending, false, true)},
		{"flushed", used(structs.IngestStateFlushed, false, false)},
		{"duplicate", used(structs.IngestStateDuplicate, false, false)},
		{"leftover relation", func(t *testing.T, id []byte) {
			err := db.Update(func(txn *badger.Txn) error {
				return txn.Set(util.AppendToBytesSlice(dbio.RelationPrefix(id), strconv.Itoa(0)), sum(id))
			})
			if err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			preferred := newUUID(t)
			c.use(t, preferred)
			eid, resumed, err := BeginAttemptID(db.DB, dbio.AttemptKey("/case/"+c.name), preferred, pendingRecord())
			if err != nil || resumed {
				t.Fatalf("BeginAttemptID: resumed %v, %v", resumed, err)
			}
			assertV7(t, eid, preferred)
		})
	}
}

func TestBeginAttemptIDPrefersResume(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default)
	key := dbio.AttemptKey("/case/e.dd")
	first, _, err := BeginAttemptID(db.DB, key, newUUID(t), pendingRecord())
	if err != nil {
		t.Fatal(err)
	}
	if err := failAttempt(db.DB, first, "the store failed"); err != nil {
		t.Fatal(err)
	}
	again, resumed, err := BeginAttemptID(db.DB, key, newUUID(t), pendingRecord())
	if err != nil || !resumed || !bytes.Equal(again, first) {
		t.Fatalf("retry: %x resumed %v, %v; want %x resumed", again, resumed, err, first)
	}
}

func TestBeginAttemptIDIgnoresNonUUID(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default)
	preferred := sum([]byte("a legacy content-hash ID"))
	eid, _, err := BeginAttemptID(db.DB, dbio.AttemptKey("/case/e.dd"), preferred, pendingRecord())
	if err != nil {
		t.Fatal(err)
	}
	assertV7(t, eid, preferred[:len(uuid.UUID{})])
}

func TestBeginAttemptResumeKeepsMerged(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default)
	src := newUUID(t)
	first := testOrigin(testPackage, src, []byte("content"))
	rec := pendingRecord()
	rec.Merged = &first
	key := dbio.MergeAttemptKey(first, rec.Size)
	eid, _, err := BeginAttemptID(db.DB, key, src, rec)
	if err != nil || !bytes.Equal(eid, src) {
		t.Fatalf("first: %x, %v", eid, err)
	}
	if err := failAttempt(db.DB, eid, "content mismatch"); err != nil {
		t.Fatal(err)
	}
	rerun := first
	rerun.Package, rerun.At = otherTestPackage, 2
	rec.Merged = &rerun
	again, resumed, err := BeginAttemptID(db.DB, dbio.MergeAttemptKey(rerun, rec.Size), src, rec)
	if err != nil || !resumed || !bytes.Equal(again, eid) {
		t.Fatalf("rerun: %x resumed %v, %v", again, resumed, err)
	}
	if e := evidenceRecord(t, db.DB, eid); e.Merged == nil || e.Merged.Package != otherTestPackage || e.Merged.At != rerun.At {
		t.Fatalf("resumed record's Merged = %+v", e.Merged)
	}
}

// An ingest without Merged writes the record and attempt key it wrote before
// merges existed, byte for byte.
func TestBeginAttemptUnchanged(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default)
	rec := pendingRecord()
	before := allKeys(t, db.DB)
	eid, _, err := BeginAttempt(db.DB, "/case/e.dd", rec)
	if err != nil {
		t.Fatal(err)
	}
	after := allKeys(t, db.DB)
	for k := range before {
		delete(after, k)
	}
	if len(after) != 2 || !bytes.Equal(after[string(dbio.AttemptKey("/case/e.dd"))], eid) {
		t.Fatalf("new keys %q, want the record and the attempt key", slices.Collect(maps.Keys(after)))
	}
	rec.Attempt = 1
	want, err := dbio.EncodeRecord(db.DB, rec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after[string(dbio.CanonicalEvidenceKey(eid))], want) {
		t.Fatal("the record is not encoded as before")
	}
	plain, err := msgpack.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(plain, []byte("merged")) {
		t.Fatal("a record without Merged encodes a merged field")
	}
}

func TestCommitEvidenceWritesLedgerStored(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: 4})
	content := testutil.RandomBytes(31, 3*4096+5)
	m := testOrigin(testPackage, newUUID(t), content)
	eid := flushMerged(t, db.DB, m, "/src/a.dd", content)
	if !bytes.Equal(eid, m.SourceEvidenceID) {
		t.Fatalf("merged under %x, want the source ID %x", eid, m.SourceEvidenceID)
	}
	commitStored(t, db.DB, eid)
	e := evidenceRecord(t, db.DB, eid)
	entry := ledgerEntry(t, db.DB, m, e.Size)
	if !bytes.Equal(entry.Target, eid) || entry.Root != e.ContentRoot || entry.Root == "" || entry.FileHash != e.FileHash || entry.Package != testPackage {
		t.Fatalf("ledger entry %+v for record %x root %q hash %q", entry, eid, e.ContentRoot, e.FileHash)
	}
	if _, present, err := MergedTarget(db.DB, m, e.Size); err != nil || !present {
		t.Fatalf("MergedTarget: present %v, %v", present, err)
	}
}

func TestCommitEvidenceWritesLedgerAliased(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: 4})
	content := testutil.RandomBytes(32, 2*4096+9)
	local := flushedEvidence(t, db.DB, "/case/a.dd", content)
	commitStored(t, db.DB, local)
	m := testOrigin(testPackage, newUUID(t), content)
	eid := flushMerged(t, db.DB, m, "/src/b.dd", content)
	res, err := CommitEvidence(db.DB, eid)
	if err != nil || res.Outcome != CommitAliased || !bytes.Equal(res.Winner, local) {
		t.Fatalf("commit: %+v, %v; want aliased to %x", res, err, local)
	}
	w := evidenceRecord(t, db.DB, local)
	size := int64(len(content))
	if entry := ledgerEntry(t, db.DB, m, size); !bytes.Equal(entry.Target, local) || entry.Root != w.ContentRoot || entry.FileHash != w.FileHash {
		t.Fatalf("ledger entry %+v, want the winner %x", entry, local)
	}
	a := aliasAt(t, db.DB, dbio.MergeAliasKey(local, m, size, "/src/b.dd"))
	if a.Merged == nil || a.Merged.Package != testPackage || !bytes.Equal(a.AttemptID, eid) {
		t.Fatalf("merged alias %+v", a)
	}
	if keyExists(t, db.DB, dbio.AliasKey(local, "/src/b.dd")) {
		t.Fatal("the merged alias was written under a local alias key")
	}
}

// A legacy winner is found through its EH entry and has no content root:
// the ledger records the winner's identity, so the item is present later.
func TestCommitEvidenceAliasedLegacyWinner(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: 4})
	content := testutil.RandomBytes(33, 2*4096+1)
	plain := base64.StdEncoding.EncodeToString(sum(content))
	legacy := []byte("legacy-evidence!")
	rec := structs.NewEvidenceFile("old.dd", 0, int64(len(content)), nil, cnst.UnknownEvidenceType, plain)
	rec.Completed, rec.IngestState = true, structs.IngestStateCompleted
	if err := dbio.CreateEvidence(db.DB, legacy, rec); err != nil {
		t.Fatal(err)
	}
	if err := dbio.AppendHashLookupUUIDByKey(db.DB, util.AppendToBytesSlice(cnst.EviFileHashLookupNamespace, plain), dbio.CanonicalEvidenceKey(legacy)); err != nil {
		t.Fatal(err)
	}
	m := testOrigin(testPackage, newUUID(t), content)
	eid := flushMerged(t, db.DB, m, "/src/new.dd", content)
	res, err := CommitEvidence(db.DB, eid)
	if err != nil || res.Outcome != CommitAliased || !bytes.Equal(res.Winner, legacy) {
		t.Fatalf("commit: %+v, %v, want aliased to the legacy evidence", res, err)
	}
	size := int64(len(content))
	entry := ledgerEntry(t, db.DB, m, size)
	if !bytes.Equal(entry.Target, legacy) || entry.Root != "" || entry.FileHash != plain {
		t.Fatalf("ledger entry %+v, want the legacy winner's empty root and hash %q", entry, plain)
	}
	if _, present, err := MergedTarget(db.DB, m, size); err != nil || !present {
		t.Fatalf("MergedTarget: present %v, %v", present, err)
	}
}

func TestCommitEvidenceMergedAliasKeepsLocal(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: 4})
	content := testutil.RandomBytes(34, 4096+3)
	const path = "/case/b.dd"
	winner := flushedEvidence(t, db.DB, "/case/a.dd", content)
	commitStored(t, db.DB, winner)
	dup := flushedEvidence(t, db.DB, path, content)
	if res, err := CommitEvidence(db.DB, dup); err != nil || res.Outcome != CommitAliased {
		t.Fatalf("local duplicate: %+v, %v", res, err)
	}
	localKey := dbio.AliasKey(winner, path)
	before := allKeys(t, db.DB)[string(localKey)]

	m := testOrigin(testPackage, newUUID(t), content)
	eid := flushMerged(t, db.DB, m, path, content)
	if res, err := CommitEvidence(db.DB, eid); err != nil || res.Outcome != CommitAliased {
		t.Fatalf("merged duplicate: %+v, %v", res, err)
	}
	if after := allKeys(t, db.DB)[string(localKey)]; !bytes.Equal(after, before) {
		t.Fatal("the merged alias changed the local alias at the same path")
	}
	aliases, err := dbio.Aliases(db.DB, winner)
	if err != nil || len(aliases) != 2 {
		t.Fatalf("aliases %+v, %v; want the local and the merged one", aliases, err)
	}
}

func TestCommitEvidenceClearsMergeAttemptKey(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: 4})
	content := testutil.RandomBytes(35, 4096)
	m := testOrigin(testPackage, newUUID(t), content)
	key := dbio.MergeAttemptKey(m, int64(len(content)))
	eid := flushMerged(t, db.DB, m, "/src/a.dd", content)
	if !keyExists(t, db.DB, key) {
		t.Fatal("no merge attempt key before the commit")
	}
	commitStored(t, db.DB, eid)
	if keyExists(t, db.DB, key) {
		t.Fatal("the commit left the merge attempt key")
	}
}

func TestCommitEvidenceWithoutMergedWritesNoLedger(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: 4})
	content := testutil.RandomBytes(36, 2*4096)
	winner := flushedEvidence(t, db.DB, "/case/a.dd", content)
	commitStored(t, db.DB, winner)
	dup := flushedEvidence(t, db.DB, "/case/b.dd", content)
	if res, err := CommitEvidence(db.DB, dup); err != nil || res.Outcome != CommitAliased {
		t.Fatalf("commit: %+v, %v", res, err)
	}
	if n := countPrefix(t, db.DB, []byte(cnst.EviMergeNamespace)); n != 0 {
		t.Fatalf("%d ledger keys after local ingests", n)
	}
	if a := aliasAt(t, db.DB, dbio.AliasKey(winner, "/case/b.dd")); a.Merged != nil || !bytes.Equal(a.AttemptID, dup) {
		t.Fatalf("local alias %+v", a)
	}
}

func TestRecoverFlushedMergeWritesLedger(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: 4})
	content := testutil.RandomBytes(37, 3*4096)
	m := testOrigin(testPackage, newUUID(t), content)
	eid := flushMerged(t, db.DB, m, "/src/a.dd", content)
	actions, err := RecoverEvidence(t.Context(), db.DB, eid, evidenceRecord(t, db.DB, eid))
	if err != nil || !slices.Contains(actions, ActionCompleted) {
		t.Fatalf("recovery: %v, %v", actions, err)
	}
	if entry, present, err := MergedTarget(db.DB, m, int64(len(content))); err != nil || !present || !bytes.Equal(entry.Target, eid) {
		t.Fatalf("MergedTarget: %+v present %v, %v", entry, present, err)
	}
}

func TestMergedTargetChecks(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default)
	const (
		size = 100
		root = "root"
		hash = "file-hash"
	)
	cases := []struct {
		name    string
		mutate  func(e *structs.EvidenceFile)
		missing bool
		want    bool
	}{
		{name: "present", mutate: func(*structs.EvidenceFile) {}, want: true},
		{name: "ingest state only", mutate: func(e *structs.EvidenceFile) { e.Completed = false }, want: true},
		{name: "missing", missing: true},
		{name: "duplicate", mutate: func(e *structs.EvidenceFile) { e.IngestState, e.Completed = structs.IngestStateDuplicate, false }},
		{name: "pending", mutate: func(e *structs.EvidenceFile) { e.IngestState, e.Completed = structs.IngestStatePending, false }},
		{name: "wrong size", mutate: func(e *structs.EvidenceFile) { e.Size++ }},
		{name: "other root", mutate: func(e *structs.EvidenceFile) { e.ContentRoot = "other" }},
		{name: "other file hash", mutate: func(e *structs.EvidenceFile) { e.FileHash = "other" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			target := newUUID(t)
			m := testOrigin(testPackage, newUUID(t), []byte(c.name))
			if !c.missing {
				rec := structs.NewEvidenceFile(c.name, 0, size, nil, cnst.UnknownEvidenceType, hash)
				rec.Completed, rec.IngestState, rec.ContentRoot = true, structs.IngestStateCompleted, root
				c.mutate(&rec)
				if err := dbio.CreateEvidence(db.DB, target, rec); err != nil {
					t.Fatal(err)
				}
			}
			entry := structs.MergeLedgerEntry{Target: target, Root: root, FileHash: hash, Package: testPackage}
			err := db.Update(func(txn *badger.Txn) error {
				return dbio.PutRecordTxn(db.DB, txn, dbio.MergeLedgerKey(m, size), entry)
			})
			if err != nil {
				t.Fatal(err)
			}
			got, present, err := MergedTarget(db.DB, m, size)
			if err != nil || present != c.want || !bytes.Equal(got.Target, target) {
				t.Fatalf("MergedTarget = %+v present %v, %v; want present %v", got, present, err, c.want)
			}
		})
	}
	if _, present, err := MergedTarget(db.DB, testOrigin(testPackage, newUUID(t), nil), size); err != nil || present {
		t.Fatalf("no ledger entry: present %v, %v", present, err)
	}
}

func TestSharedIDPresent(t *testing.T) {
	t.Parallel()
	source := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: 4})
	target := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: 4})
	content := testutil.RandomBytes(38, 3*4096+17)
	id := flushedEvidence(t, source.DB, "/case/a.dd", content)
	commitStored(t, source.DB, id)
	got, _, err := BeginAttemptID(target.DB, dbio.AttemptKey("/case/a.dd"), id, attemptRecord(target.DB, "/case/a.dd", content))
	if err != nil || !bytes.Equal(got, id) {
		t.Fatalf("target attempt %x, %v", got, err)
	}
	flushChunks(t, target.DB, id, content, false)
	commitStored(t, target.DB, id)
	src := evidenceRecord(t, source.DB, id)

	check := func(name string, src structs.EvidenceFile, want bool) {
		t.Helper()
		from, present, err := SharedIDPresent(t.Context(), target.DB, source.DB, id, src)
		if err != nil || present != want {
			t.Fatalf("%s: present %v, %v; want %v", name, present, err, want)
		}
		if present && from != nil {
			t.Fatalf("%s: evidence stored here reported as merged from %+v", name, from)
		}
	}
	check("same content", src, true)

	other := src
	other.PlainHash = base64.StdEncoding.EncodeToString(sum([]byte("other")))
	check("other plain hash", other, false)
	other = src
	other.ChunkSize *= 2
	check("other chunk size", other, false)
	other = src
	other.FileHashAlgo = dbmeta.FileHashSHA3
	check("other file hash algorithm", other, false)
	other = src
	other.Size++
	check("other size", other, false)
	other = src
	other.HashStrategy = "other-strategy"
	check("V2 records with another strategy and the same plain hash", other, true)
	if _, missing, err := SharedIDPresent(t.Context(), target.DB, source.DB, newUUID(t), src); err != nil || missing {
		t.Fatalf("an ID the target lacks: present %v, %v", missing, err)
	}

	merged := testOrigin(testPackage, id, content)
	err = dbio.UpdateEvidence(target.DB, id, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		e.Merged = &merged
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if from, present, err := SharedIDPresent(t.Context(), target.DB, source.DB, id, src); err != nil || !present || from == nil || from.Package != testPackage {
		t.Fatalf("evidence merged from a package: from %+v, present %v, %v", from, present, err)
	}
	err = dbio.UpdateEvidence(target.DB, id, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		e.Merged = nil
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	err = dbio.UpdateEvidence(target.DB, id, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		e.ContentModel = ""
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	legacy := src
	legacy.ContentModel = ""
	check("legacy records of one strategy", legacy, true)
	other = legacy
	other.HashStrategy = "other-strategy"
	check("legacy records of other strategies", other, false)
	other = legacy
	other.FileHash = base64.StdEncoding.EncodeToString(sum([]byte("other")))
	check("legacy records of other file hashes", other, false)

	err = source.Update(func(txn *badger.Txn) error {
		return txn.Set(util.AppendToBytesSlice(dbio.RelationPrefix(id), strconv.Itoa(0)), sum([]byte("another chunk")))
	})
	if err != nil {
		t.Fatal(err)
	}
	check("one relation names another chunk", legacy, false)
}

func TestCarryAliasesIdempotentAndBatched(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default)
	winner := newUUID(t)
	content := []byte("carried")
	size := int64(len(content))
	m := testOrigin(testPackage, newUUID(t), content)
	as := make([]structs.AliasRecord, deleteBatch+3)
	for i := range as {
		path := "/src/alias-" + strconv.Itoa(i)
		as[i] = structs.AliasRecord{Name: path, Path: path, Size: size, AttemptID: newUUID(t), At: 1}
	}
	local := structs.AliasRecord{Name: as[0].Path, Path: as[0].Path, Size: size, AttemptID: newUUID(t)}
	if err := WriteAlias(db.DB, winner, local); err != nil {
		t.Fatal(err)
	}
	localBefore := allKeys(t, db.DB)[string(dbio.AliasKey(winner, local.Path))]

	n, err := CarryAliases(db.DB, winner, m, size, as)
	if err != nil || n != len(as) {
		t.Fatalf("first carry wrote %d, %v; want %d", n, err, len(as))
	}
	if n, err := CarryAliases(db.DB, winner, m, size, as); err != nil || n != 0 {
		t.Fatalf("second carry wrote %d, %v; want 0", n, err)
	}
	if got := countPrefix(t, db.DB, dbio.AliasPrefix(winner)); got != len(as)+1 {
		t.Fatalf("%d aliases, want %d carried and the local one", got, len(as))
	}
	if after := allKeys(t, db.DB)[string(dbio.AliasKey(winner, local.Path))]; !bytes.Equal(after, localBefore) {
		t.Fatal("carrying changed the local alias at the same path")
	}
	last := as[len(as)-1]
	a := aliasAt(t, db.DB, dbio.MergeAliasKey(winner, m, size, last.Path))
	if a.Merged == nil || a.Merged.Package != testPackage || a.AttemptID != nil || a.Path != last.Path {
		t.Fatalf("carried alias %+v", a)
	}
}

func TestWalkEvidenceReportsUndecodable(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default)
	good := newUUID(t)
	if err := dbio.CreateEvidence(db.DB, good, pendingRecord()); err != nil {
		t.Fatal(err)
	}
	broken, other := newUUID(t), newUUID(t)
	var bare structs.EvidenceFile
	bare.Name = "not evidence"
	notEvidence, err := dbio.EncodeRecord(db.DB, bare)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(txn *badger.Txn) error {
		if err := txn.Set(dbio.CanonicalEvidenceKey(broken), []byte{msgpackNeverUsed}); err != nil {
			return err
		}
		return txn.Set(dbio.CanonicalEvidenceKey(other), notEvidence)
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]error{}
	err = WalkEvidence(db.DB, func(eid []byte, _ structs.EvidenceFile, err error) error {
		seen[string(eid)] = err
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("walked %d records, want the evidence and the undecodable one", len(seen))
	}
	if err, ok := seen[string(good)]; !ok || err != nil {
		t.Fatalf("evidence: seen %v, %v", ok, err)
	}
	if err, ok := seen[string(broken)]; !ok || err == nil {
		t.Fatalf("undecodable record: seen %v, err %v", ok, err)
	}
}

// msgpackNeverUsed is the one msgpack type byte no value starts with.
const msgpackNeverUsed = 0xc1

func TestChunkPassSecondHash(t *testing.T) {
	t.Parallel()
	content := testutil.RandomBytes(39, 5*4096+77)
	want := sha3.Sum256(content)
	for _, mode := range []string{cnst.StorePipelineBatch, cnst.StorePipelineStaged} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			tun := testdb.Tuning()
			tun.Pipeline = mode
			db := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: 4, Tuning: &tun})
			if err := util.EnsureBlobPath(db.Opts().Dir); err != nil {
				t.Fatal(err)
			}
			pass := func(second bool) ChunkResult {
				p := ChunkPass{DB: db.DB, ID: newUUID(t), Source: bytes.NewReader(content), Size: int64(len(content)), Quiet: true}
				if second {
					p.SecondHash = func() hash.Hash { return sha3.New256() }
				}
				res, err := IngestChunks(t.Context(), p)
				if err != nil {
					t.Fatal(err)
				}
				return res
			}
			without, with := pass(false), pass(true)
			if without.SecondPlain != nil {
				t.Fatalf("SecondPlain %x without a second hash", without.SecondPlain)
			}
			if !bytes.Equal(with.SecondPlain, want[:]) {
				t.Fatalf("SecondPlain %x, want %x", with.SecondPlain, want)
			}
			if !bytes.Equal(with.PlainHash, without.PlainHash) || !bytes.Equal(with.Root, without.Root) || with.Chunks != without.Chunks {
				t.Fatal("a second hash changed the pass's own result")
			}
		})
	}
}
