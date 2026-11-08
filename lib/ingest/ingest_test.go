package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/fsfixture"
	"indicer/internal/testutil/leakcheck"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/index"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

func TestMain(m *testing.M) {
	code := leakcheck.Run(m)
	fsfixture.Cleanup()
	os.Exit(code)
}

// openDB opens a database in 64 KiB chunks, which keep the multi-megabyte
// cases to a few dozen blobs.
func openDB(t *testing.T) *badger.DB {
	t.Helper()
	return openDBWith(t, testdb.Options{ChunkKiB: 64})
}

// openDBStrategy opens a database like openDB that stores with the hash
// strategy given.
func openDBStrategy(t *testing.T, strategy string) *badger.DB {
	t.Helper()
	tun := testdb.Tuning()
	tun.HashStrategy = strategy
	return openDBWith(t, testdb.Options{ChunkKiB: 64, Tuning: &tun})
}

// runWith ingests req into db, as Run does, in a session whose passes edit
// replaced.
func runWith(ctx context.Context, db *badger.DB, req Request, edit func(*passes)) (Result, error) {
	s := NewSession(db)
	edit(&s.passes)
	res, err := s.Run(ctx, req)
	return res, s.closeWith(err)
}

// openDBWith opens a database with the options given.
func openDBWith(t *testing.T, o testdb.Options) *badger.DB {
	t.Helper()
	d := testdb.Open(t, testdb.Default, o)
	if err := util.EnsureBlobPath(d.Root); err != nil {
		t.Fatal(err)
	}
	return d.DB
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	for _, m := range fsfixture.All(t) {
		if m.Name == name {
			return fsfixture.Open(t, m)
		}
	}
	t.Fatalf("no fixture %s", name)
	return ""
}

// evidenceRecords returns every evidence record in db.
func evidenceRecords(t *testing.T, db *badger.DB) map[string]structs.EvidenceFile {
	t.Helper()
	out := map[string]structs.EvidenceFile{}
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(cnst.EviFileNamespace)
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			id := dbio.EvidenceRawID(it.Item().KeyCopy(nil))
			e, err := dbio.GetEvidenceTxn(txn, id)
			if err != nil {
				return err
			}
			out[string(id)] = e
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func restoreBytes(t *testing.T, db *badger.DB, eid []byte) []byte {
	t.Helper()
	l, err := dbio.OpenLogical(db, dbio.CanonicalEvidenceKey(eid), dbio.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, l.Size())
	if n, err := l.ReadAt(b, 0); int64(n) != l.Size() {
		t.Fatalf("read %d of %d: %v", n, l.Size(), err)
	}
	return b
}

// TestReingestIsAnAlias (D1): storing content that is already stored stores
// nothing and records an alias, for every hash strategy, for content small
// enough that the fingerprint is exact and for content whose root must be
// confirmed.
func TestReingestIsAnAlias(t *testing.T) {
	t.Parallel()
	for _, strategy := range []string{cnst.SyncHashStrategy, cnst.AsyncHashStrategy, cnst.HochoHashStrategy} {
		for _, size := range []int{64 << 10, 5 << 20} {
			t.Run(fmt.Sprintf("%s/%dKiB", strategy, size>>10), func(t *testing.T) {
				db := openDBStrategy(t, strategy)
				dir := t.TempDir()
				first := testutil.WriteRandomFile(t, dir, "first.bin", 1, size)
				content, _ := os.ReadFile(first)
				second := filepath.Join(dir, "copy-of-first.bin")
				if err := os.WriteFile(second, content, 0o600); err != nil {
					t.Fatal(err)
				}

				r1, err := Run(t.Context(), db, Request{Path: first, Decode: true, Quiet: true})
				if err != nil || r1.Outcome != store.CommitStored {
					t.Fatalf("first ingest: %+v, %v", r1, err)
				}
				r2, err := Run(t.Context(), db, Request{Path: second, Decode: true, Quiet: true})
				if err != nil || r2.Outcome != store.CommitAliased || !bytes.Equal(r2.EvidenceID, r1.EvidenceID) {
					t.Fatalf("second ingest: %+v, %v; want an alias of %x", r2, err, r1.EvidenceID)
				}
				if len(r2.Attempt) != 0 {
					t.Errorf("the second ingest stored under %x; the stored content should have been found first", r2.Attempt)
				}
				if recs := evidenceRecords(t, db); len(recs) != 1 {
					t.Errorf("%d evidence records, want 1", len(recs))
				}
				aliases, err := dbio.Aliases(db, r1.EvidenceID)
				if err != nil || len(aliases) != 1 || aliases[0].Name != "copy-of-first.bin" {
					t.Errorf("aliases %+v, %v", aliases, err)
				}
				// Ingesting the same file again rewrites the same alias.
				if _, err := Run(t.Context(), db, Request{Path: second, Decode: true, Quiet: true}); err != nil {
					t.Fatal(err)
				}
				if aliases, _ := dbio.Aliases(db, r1.EvidenceID); len(aliases) != 1 {
					t.Errorf("%d aliases after the same file was stored twice, want 1", len(aliases))
				}
				if !bytes.Equal(restoreBytes(t, db, r1.EvidenceID), content) {
					t.Error("the stored evidence does not read back")
				}
			})
		}
	}
}

// TestConcurrentIngestsHaveOneWinner (D1, I6): ingests of the same content
// that run at once each store it, and the commit makes exactly one the
// evidence; the others become aliases and their records are removed.
func TestConcurrentIngestsHaveOneWinner(t *testing.T) {
	t.Parallel()
	db := openDBStrategy(t, cnst.AsyncHashStrategy)
	dir := t.TempDir()
	src := testutil.WriteRandomFile(t, dir, "src.bin", 7, 300<<10)
	content, _ := os.ReadFile(src)
	const n = 4
	paths := make([]string, n)
	for i := range paths {
		paths[i] = filepath.Join(dir, "copy"+string(rune('a'+i))+".bin")
		if err := os.WriteFile(paths[i], content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	results := make([]Result, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = Run(context.Background(), db, Request{Path: paths[i], Decode: true, NoIndex: true, Quiet: true})
		}()
	}
	wg.Wait()
	var winner []byte
	stored := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("ingest %d: %v", i, errs[i])
		}
		if results[i].Outcome == store.CommitStored {
			stored++
			winner = results[i].EvidenceID
		}
	}
	if stored != 1 {
		t.Fatalf("%d ingests stored the content, want exactly 1", stored)
	}
	for i, r := range results {
		if !bytes.Equal(r.EvidenceID, winner) {
			t.Errorf("ingest %d names evidence %x, the winner is %x", i, r.EvidenceID, winner)
		}
	}
	if recs := evidenceRecords(t, db); len(recs) != 1 {
		t.Errorf("%d evidence records after the duplicates were cleaned up, want 1", len(recs))
	}
	if aliases, _ := dbio.Aliases(db, winner); len(aliases) != n-1 {
		t.Errorf("%d aliases, want %d", len(aliases), n-1)
	}
	if !bytes.Equal(restoreBytes(t, db, winner), content) {
		t.Error("the winner does not read back")
	}
}

// TestStorageFailureResumes: an ingest whose chunk pass fails is recorded
// failed with its error, stops its index pass, and the next ingest of the
// same file carries on under the same ID.
func TestStorageFailureResumes(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	path := fixture(t, "fat/fat16")
	boom := errors.New("simulated read failure")
	var indexErr error
	r1, err := runWith(t.Context(), db, Request{Path: path, Decode: true, Quiet: true}, func(p *passes) {
		p.index = func(ctx context.Context, o index.Options) (index.Result, error) {
			res, err := index.Run(ctx, o)
			indexErr = err
			return res, err
		}
		p.chunks = func(context.Context, store.ChunkPass) (store.ChunkResult, error) {
			return store.ChunkResult{}, boom
		}
	})
	if !errors.Is(err, boom) {
		t.Fatalf("ingest with a failing chunk pass: %v", err)
	}
	e, err := dbio.GetEvidenceFile(r1.Attempt, db)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Failed || e.Completed || e.LastError == "" || e.Index == nil || e.Index.State == structs.IndexComplete {
		t.Fatalf("failed attempt: failed %v completed %v error %q index %+v", e.Failed, e.Completed, e.LastError, e.Index)
	}
	_ = indexErr // the index pass may have finished before storage failed

	r2, err := Run(t.Context(), db, Request{Path: path, Decode: true, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Resumed || !bytes.Equal(r2.Attempt, r1.Attempt) || r2.Outcome != store.CommitStored {
		t.Fatalf("second ingest: resumed %v attempt %x (first %x) outcome %s", r2.Resumed, r2.Attempt, r1.Attempt, r2.Outcome)
	}
	if e, _ := dbio.GetEvidenceFile(r2.Attempt, db); !e.Completed || e.Failed || e.LastError != "" || e.Attempt != 2 || e.Index.State != structs.IndexComplete {
		t.Fatalf("resumed evidence: completed %v failed %v error %q attempt %d index %+v", e.Completed, e.Failed, e.LastError, e.Attempt, e.Index)
	}
}

// TestCommitClearsAttemptKey: a committed ingest leaves no attempt key, also
// when it records a source path other than the file it read (server uploads
// read a part file and record the client's path).
func TestCommitClearsAttemptKey(t *testing.T) {
	t.Parallel()
	for _, sourcePath := range []string{"", "client/upload.img"} {
		t.Run("source="+sourcePath, func(t *testing.T) {
			db := openDB(t)
			path := fixture(t, "fat/fat16")
			if _, err := Run(t.Context(), db, Request{Path: path, SourcePath: sourcePath, Decode: true, NoIndex: true, Quiet: true}); err != nil {
				t.Fatal(err)
			}
			err := db.View(func(txn *badger.Txn) error {
				it := txn.NewIterator(badger.IteratorOptions{Prefix: []byte(cnst.EviAttemptNamespace)})
				defer it.Close()
				for it.Rewind(); it.Valid(); it.Next() {
					t.Errorf("attempt key left after commit: %x", it.Item().Key())
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestIndexFailureKeepsStorage: an index pass that fails does not stop the
// evidence being stored and committed; the ingest reports the index
// incomplete, and the evidence records why.
func TestIndexFailureKeepsStorage(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	path := fixture(t, "fat/fat12")
	boom := errors.New("simulated index failure")
	res, err := runWith(t.Context(), db, Request{Path: path, Decode: true, Quiet: true}, func(p *passes) {
		p.index = func(context.Context, index.Options) (index.Result, error) {
			return index.Result{}, boom
		}
	})
	var incomplete *IndexIncompleteError
	if !errors.As(err, &incomplete) || !errors.Is(err, boom) || incomplete.State != structs.IndexFailed {
		t.Fatalf("ingest: %v, want an incomplete index wrapping the failure", err)
	}
	e, _ := dbio.GetEvidenceFile(res.EvidenceID, db)
	if res.Outcome != store.CommitStored || !e.Completed || e.Index == nil || e.Index.State != structs.IndexFailed || e.Index.Reason == "" {
		t.Fatalf("outcome %s, completed %v, index %+v", res.Outcome, e.Completed, e.Index)
	}
}

// TestEWFDigestMismatch: an EWF set whose stored digest does not match is
// stored and flagged, or, with RequireVerified, not committed.
func TestEWFDigestMismatch(t *testing.T) {
	t.Parallel()
	path := fixture(t, "ewf/fat12_multiseg")
	t.Run("verified", func(t *testing.T) {
		db := openDB(t)
		res, err := Run(t.Context(), db, Request{Path: path, Decode: true, Quiet: true})
		if err != nil {
			t.Fatal(err)
		}
		e, _ := dbio.GetEvidenceFile(res.EvidenceID, db)
		if e.Source == nil || e.Source.EWF == nil || e.Source.EWF.Result != structs.EWFVerified || len(e.Source.EWF.MD5) == 0 || !bytes.Equal(e.Source.EWF.MD5, e.Source.EWF.StoredMD5) {
			t.Fatalf("EWF check %+v", e.Source)
		}
		if e.Source.Format != structs.SourceEWF || len(e.Source.Members) == 0 {
			t.Fatalf("source %+v", e.Source)
		}
		if res.Source.EWF == nil || res.Source.EWF.Result != structs.EWFVerified {
			t.Fatalf("the result does not report the check: %+v", res.Source.EWF)
		}
	})
	// The chunk pass finds the stored digest does not match.
	mismatch := func(p *passes) {
		p.chunks = func(ctx context.Context, cp store.ChunkPass) (store.ChunkResult, error) {
			res, err := store.IngestChunks(ctx, cp)
			if res.EWF != nil {
				res.EWF.MD5Match = false
			}
			return res, err
		}
	}
	t.Run("flagged", func(t *testing.T) {
		db := openDB(t)
		res, err := runWith(t.Context(), db, Request{Path: path, Decode: true, Quiet: true}, mismatch)
		var mis *DigestMismatchError
		if !errors.As(err, &mis) || !mis.Stored || res.Outcome != store.CommitStored {
			t.Fatalf("ingest: %v, outcome %s; want stored and flagged", err, res.Outcome)
		}
		if e, _ := dbio.GetEvidenceFile(res.EvidenceID, db); !e.Completed || e.Source.EWF.Result != structs.EWFMismatch {
			t.Fatalf("evidence: completed %v, EWF %+v", e.Completed, e.Source.EWF)
		}
	})
	t.Run("required", func(t *testing.T) {
		db := openDB(t)
		res, err := runWith(t.Context(), db, Request{Path: path, Decode: true, Quiet: true, RequireVerified: true}, mismatch)
		var mis *DigestMismatchError
		if !errors.As(err, &mis) || mis.Stored || res.Outcome != "" {
			t.Fatalf("ingest: %v, outcome %q; want a failure", err, res.Outcome)
		}
		if e, _ := dbio.GetEvidenceFile(res.Attempt, db); e.Completed || !e.Failed {
			t.Fatalf("evidence: completed %v failed %v", e.Completed, e.Failed)
		}
	})
}
