package store

import (
	"bytes"
	"encoding/base64"
	"errors"
	"sync"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// flushedEvidence stores content as a new evidence from path and leaves it
// FLUSHED, as an ingest does before its commit.
func flushedEvidence(t *testing.T, db *badger.DB, path string, content []byte) []byte {
	t.Helper()
	if err := util.EnsureBlobPath(db.Opts().Dir); err != nil {
		t.Fatal(err)
	}
	rec := structs.NewEvidenceFile("evidence", 0, int64(len(content)), nil, cnst.UnknownEvidenceType, "")
	rec.ContentModel = structs.ContentModelV2
	rec.ChunkSize = cnst.ChonkSize
	rec.Source = &structs.EvidenceSource{Format: structs.SourceRaw, Members: []structs.SourceMember{{Path: path, Size: int64(len(content))}}}
	rec.Fingerprint = "fp-" + base64.StdEncoding.EncodeToString(sum(content))
	eid, _, err := BeginAttempt(db, path, rec)
	if err != nil {
		t.Fatal(err)
	}
	res, err := IngestChunks(t.Context(), ChunkPass{DB: db, ID: eid, Source: bytes.NewReader(content), Size: int64(len(content)), Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	err = dbio.UpdateEvidence(db, eid, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		e.PlainHash = base64.StdEncoding.EncodeToString(res.PlainHash)
		e.ContentRoot = base64.StdEncoding.EncodeToString(res.Root)
		e.FileHash = e.PlainHash
		e.IngestState = structs.IngestStateFlushed
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return eid
}

func countPrefix(t *testing.T, db *badger.DB, prefix []byte) int {
	t.Helper()
	n := 0
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
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

// The second ingest of the same content becomes an alias of the first, and
// its relations go (D1, ING-6).
func TestCommitAliasesStoredContent(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	util.SetChonkSize(4)
	prev := cnst.ENABLEREVREL
	cnst.ENABLEREVREL = true
	t.Cleanup(func() { cnst.ENABLEREVREL = prev })
	content := testutil.RandomBytes(11, 5*4096+7)

	first := flushedEvidence(t, db.DB, "/case/a.dd", content)
	res, err := CommitEvidence(db.DB, first)
	if err != nil || res.Outcome != CommitStored || !bytes.Equal(res.Winner, first) {
		t.Fatalf("first commit: %+v, %v", res, err)
	}

	second := flushedEvidence(t, db.DB, "/case/copy.dd", content)
	if bytes.Equal(first, second) {
		t.Fatal("a second path resumed the first attempt")
	}
	res, err = CommitEvidence(db.DB, second)
	if err != nil || res.Outcome != CommitAliased || !bytes.Equal(res.Winner, first) {
		t.Fatalf("second commit: %+v, %v (want aliased to %x)", res, err, first)
	}
	again, err := CommitEvidence(db.DB, second)
	if err != nil || again.Outcome != CommitAliased {
		t.Fatalf("commit again: %+v, %v", again, err)
	}
	aliases, err := dbio.Aliases(db.DB, first)
	if err != nil || len(aliases) != 1 || aliases[0].Path != "/case/copy.dd" || !bytes.Equal(aliases[0].AttemptID, second) {
		t.Fatalf("aliases %+v, %v", aliases, err)
	}

	relsSecond := countPrefix(t, db.DB, dbio.RelationPrefix(second))
	if relsSecond != 6 {
		t.Fatalf("duplicate has %d relations before cleanup, want 6", relsSecond)
	}
	revBefore := countPrefix(t, db.DB, []byte(cnst.ReverseRelationAppendNamespace))
	if err := CleanupDuplicate(t.Context(), db.DB, second); err != nil {
		t.Fatal(err)
	}
	if n := countPrefix(t, db.DB, dbio.RelationPrefix(second)); n != 0 {
		t.Fatalf("%d relations of the duplicate left", n)
	}
	if n := countPrefix(t, db.DB, dbio.RelationPrefix(first)); n != 6 {
		t.Fatalf("winner has %d relations after cleanup, want 6", n)
	}
	if revAfter := countPrefix(t, db.DB, []byte(cnst.ReverseRelationAppendNamespace)); revAfter != revBefore-6 {
		t.Fatalf("reverse relations %d -> %d, want 6 fewer", revBefore, revAfter)
	}
	if _, err := dbio.GetEvidenceFile(second, db.DB); !errors.Is(err, badger.ErrKeyNotFound) {
		t.Fatalf("duplicate record still there: %v", err)
	}
	if err := CleanupDuplicate(t.Context(), db.DB, second); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
	if w, err := StoredByIdentity(db.DB, int64(len(content)), base64.StdEncoding.EncodeToString(rootOf(t, db.DB, content)), ""); err != nil || !bytes.Equal(w, first) {
		t.Fatalf("StoredByIdentity = %x, %v", w, err)
	}
}

func rootOf(t *testing.T, db *badger.DB, content []byte) []byte {
	t.Helper()
	s, err := HashContent(t.Context(), db, bytes.NewReader(content), int64(len(content)), false)
	if err != nil {
		t.Fatal(err)
	}
	return s.Root
}

// Concurrent commits of the same content leave exactly one winner.
func TestCommitConcurrentSingleWinner(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	util.SetChonkSize(4)
	content := testutil.RandomBytes(12, 3*4096)
	var ids [][]byte
	for i := range 6 {
		ids = append(ids, flushedEvidence(t, db.DB, "/case/"+string(rune('a'+i)), content))
	}
	results := make([]CommitResult, len(ids))
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = CommitEvidence(db.DB, id)
		}()
	}
	wg.Wait()
	var winners [][]byte
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("commit %d: %v", i, errs[i])
		}
		if r.Outcome == CommitStored {
			winners = append(winners, r.Winner)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("%d winners, want 1", len(winners))
	}
	for i, r := range results {
		if !bytes.Equal(r.Winner, winners[0]) {
			t.Fatalf("commit %d names winner %x, want %x", i, r.Winner, winners[0])
		}
	}
}

// Evidence stored before W6 has no content root; a later ingest of the same
// bytes still finds it by its plain hash.
func TestCommitFindsLegacyWinner(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	util.SetChonkSize(4)
	content := testutil.RandomBytes(13, 2*4096+1)
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
	eid := flushedEvidence(t, db.DB, "/case/new.dd", content)
	res, err := CommitEvidence(db.DB, eid)
	if err != nil || res.Outcome != CommitAliased || !bytes.Equal(res.Winner, legacy) {
		t.Fatalf("commit: %+v, %v, want aliased to the legacy evidence", res, err)
	}
}

// Cleanup removes a duplicate's index records too: an index pass that ran
// alongside its chunk pass wrote them before the commit decided.
func TestCleanupDuplicateRemovesIndex(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	util.SetChonkSize(4)
	content := testutil.RandomBytes(14, 4096)
	first := flushedEvidence(t, db.DB, "/a", content)
	if _, err := CommitEvidence(db.DB, first); err != nil {
		t.Fatal(err)
	}
	dup := flushedEvidence(t, db.DB, "/b", content)

	pid := []byte("partition-0001--")
	iid := []byte("indexed-0001----")
	pkey := dbio.CanonicalPartitionKey(pid)
	ikey := dbio.CanonicalIndexedKey(iid)
	irec := structs.NewIndexedFile("/x.txt", 0, 10, "", false, "ihash")
	irec.EvidenceID, irec.PartitionID, irec.Path = dup, pid, "/x.txt"
	obj := []byte("object-0001-----")
	irec.Content = &structs.ContentRef{Kind: structs.ContentMaterialized, Located: 10, ObjectID: obj}
	batch := db.NewWriteBatch()
	if err := dbio.SetIndexedFile(ikey, irec, batch); err != nil {
		t.Fatal(err)
	}
	if err := batch.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := IngestChunks(t.Context(), ChunkPass{DB: db.DB, ID: obj, Source: bytes.NewReader(content[:10]), Size: 10, Quiet: true}); err != nil {
		t.Fatal(err)
	}
	prec := structs.NewPartitionFile("p0", 0, 4096, nil, "phash")
	members := map[string]structs.InternalOffset{base64.StdEncoding.EncodeToString(iid): {Start: 0, End: 9}}
	if err := dbio.SetPartitionWithMembers(db.DB, pkey, prec, members); err != nil {
		t.Fatal(err)
	}
	if err := dbio.AppendHashLookupUUIDByKey(db.DB, util.AppendToBytesSlice(cnst.IdxFileHashLookupNamespace, "ihash"), ikey); err != nil {
		t.Fatal(err)
	}
	err := dbio.UpdateEvidence(db.DB, dup, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		e.InternalObjects = map[string]structs.InternalOffset{base64.StdEncoding.EncodeToString(pid): {Start: 0, End: 4095}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if res, err := CommitEvidence(db.DB, dup); err != nil || res.Outcome != CommitAliased {
		t.Fatalf("commit: %+v, %v", res, err)
	}
	if err := CleanupDuplicate(t.Context(), db.DB, dup); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range [][]byte{pkey, ikey, dbio.PartitionMemberPrefix(pkey), dbio.RelationPrefix(obj), dbio.ObjectKey(obj),
		dbio.HashLookupMemberKey(util.AppendToBytesSlice(cnst.IdxFileHashLookupNamespace, "ihash"), ikey)} {
		if n := countPrefix(t, db.DB, prefix); n != 0 {
			t.Fatalf("%d keys under %q left", n, prefix)
		}
	}
}

// An attempt that failed resumes under its ID when the same path is stored
// again with the same content; changed content starts a new evidence.
func TestBeginAttemptResumes(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	rec := structs.NewEvidenceFile("e", 0, 100, nil, cnst.UnknownEvidenceType, "")
	rec.ContentModel, rec.Fingerprint, rec.ChunkSize = structs.ContentModelV2, "fp", 4096
	a, resumed, err := BeginAttempt(db.DB, "/case/e.dd", rec)
	if err != nil || resumed {
		t.Fatalf("first: resumed %v, %v", resumed, err)
	}
	if err := failAttempt(db.DB, a, "the store failed"); err != nil {
		t.Fatal(err)
	}
	b, resumed, err := BeginAttempt(db.DB, "/case/e.dd", rec)
	if err != nil || !resumed || !bytes.Equal(a, b) {
		t.Fatalf("retry: %x resumed %v, %v; want %x resumed", b, resumed, err, a)
	}
	e, err := dbio.GetEvidenceFile(b, db.DB)
	if err != nil || e.Attempt != 2 || e.Failed || e.IngestState != structs.IngestStatePending {
		t.Fatalf("resumed record %+v, %v", e, err)
	}
	changed := rec
	changed.Size = 200
	c, resumed, err := BeginAttempt(db.DB, "/case/e.dd", changed)
	if err != nil || resumed || bytes.Equal(c, a) {
		t.Fatalf("changed content: %x resumed %v, %v", c, resumed, err)
	}
}
