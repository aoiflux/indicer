package fts_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"indicer/internal/testutil/fsfixture"
	"indicer/internal/testutil/leakcheck"
	"indicer/internal/testutil/testdb"
	"indicer/lib/dbio"
	"indicer/lib/fts"
	"indicer/lib/index"
	"indicer/lib/ingest"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/blevesearch/bleve/v2"
	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
)

func TestMain(m *testing.M) {
	code := leakcheck.Run(m)
	fsfixture.Cleanup()
	os.Exit(code)
}

// storeFixture stores a fixture, with the full-text index when withFTS.
func storeFixture(t *testing.T, name string, withFTS bool) (*testdb.DB, []byte) {
	t.Helper()
	d := testdb.Open(t, testdb.Default)
	util.SetChonkSize(64)
	if err := util.EnsureBlobPath(d.Root); err != nil {
		t.Fatal(err)
	}
	var path string
	for _, m := range fsfixture.All(t) {
		if m.Name == name {
			path = fsfixture.Open(t, m)
		}
	}
	if path == "" {
		t.Fatalf("no fixture %s", name)
	}
	res, err := ingest.Run(t.Context(), d.DB, ingest.Request{Path: path, Decode: true, Quiet: true, FTS: withFTS})
	if err != nil {
		t.Fatal(err)
	}
	return d, res.EvidenceID
}

func jobsOf(t *testing.T, db *badger.DB, eid []byte) map[string]fts.IndexedFileJob {
	t.Helper()
	jobs := map[string]fts.IndexedFileJob{}
	err := index.ForEachIndexed(t.Context(), db, eid, func(key []byte, rec structs.IndexedFile) error {
		jobs[rec.Path] = fts.IndexedFileJob{FileID: string(key), Name: rec.Path}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func contentOf(t *testing.T, db *badger.DB, key string) string {
	t.Helper()
	l, err := dbio.OpenLogical(db, []byte(key), dbio.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := l.Stream(t.Context(), func(p []byte) error { b.Write(p); return nil }); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func search(t *testing.T, db *badger.DB, mode fts.QueryMode, terms ...string) map[string]struct{} {
	t.Helper()
	c, err := fts.SearchCandidates(t.Context(), db, terms, mode, 0)
	if err != nil {
		t.Fatalf("%q: %v", terms, err)
	}
	return c.IDs()
}

// TestCandidatesMatchNameAndText: a file is offered for a word of its path,
// a word of its text, and both together (one document holds both); an AND
// query needs every term, an OR query any.
func TestCandidatesMatchNameAndText(t *testing.T) {
	d, eid := storeFixture(t, "fat/fat32_fsagen", true)
	jobs := jobsOf(t, d.DB, eid)
	readme, ok := jobs["/docs/readme.txt"]
	if !ok {
		t.Fatalf("no /docs/readme.txt among %d files", len(jobs))
	}
	word := strings.FieldsFunc(strings.ToLower(contentOf(t, d.DB, readme.FileID)), func(r rune) bool { return !unicode.IsLetter(r) })
	if len(word) == 0 {
		t.Fatal("the readme holds no word")
	}
	for _, terms := range [][]string{{"readme"}, {word[0]}, {"readme", word[0]}, {"docs", "readme"}} {
		if _, ok := search(t, d.DB, fts.QueryModeAnd, terms...)[readme.FileID]; !ok {
			t.Errorf("%q does not offer /docs/readme.txt", terms)
		}
	}
	if _, ok := search(t, d.DB, fts.QueryModeAnd, "readme", "nosuchwordanywhere")[readme.FileID]; ok {
		t.Error("an AND query with a word nowhere offers the readme")
	}
	if _, ok := search(t, d.DB, fts.QueryModeOr, "readme", "nosuchwordanywhere")[readme.FileID]; !ok {
		t.Error("an OR query does not offer the readme")
	}
}

// TestSearchCandidatesTreatsTermsLiterally (SRC-3): terms are never parsed
// as query syntax, so a term with its operators is not an error.
func TestSearchCandidatesTreatsTermsLiterally(t *testing.T) {
	d, _ := storeFixture(t, "fat/fat16", true)
	for _, term := range []string{"name:foo", "c++", `"unbalanced`, `+-=&|><!(){}[]^~*?:\/`, "AND OR NOT", "foo*"} {
		if _, err := fts.SearchCandidates(t.Context(), d.DB, []string{term}, fts.QueryModeAnd, 0); err != nil {
			t.Errorf("%q: %v", term, err)
		}
	}
}

// TestConcurrentUseSharesOneIndex (SRC-7): searches and indexing in one
// process share one open index, instead of each call opening it again and
// waiting for the file lock of the others' opens.
func TestConcurrentUseSharesOneIndex(t *testing.T) {
	d, eid := storeFixture(t, "fat/fat16", true)
	var jobs []fts.IndexedFileJob
	for _, j := range jobsOf(t, d.DB, eid) {
		jobs = append(jobs, j)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() {
			for range 3 {
				var err error
				if i%2 == 0 {
					_, err = fts.IndexFiles(ctx, d.DB, jobs)
				} else {
					_, err = fts.SearchCandidates(ctx, d.DB, []string{"txt"}, fts.QueryModeAnd, 0)
				}
				if err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestIndexHeldElsewhereTimesOut (SRC-7): an index another process holds is
// an error after a short wait, not a hang; before, the open waited for as
// long as the other holder kept it.
func TestIndexHeldElsewhereTimesOut(t *testing.T) {
	d, _ := storeFixture(t, "fat/fat16", true)
	other, err := bleve.Open(util.FTSPath(d.Root))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	done := make(chan error, 1)
	go func() {
		_, err := fts.SearchCandidates(context.Background(), d.DB, []string{"txt"}, fts.QueryModeAnd, 0)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a search opened an index another holder has locked")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a search waits for an index another holder has locked")
	}
}

func reopenReadOnly(t *testing.T, d *testdb.DB) *badger.DB {
	t.Helper()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	opts := dbio.BadgerOptions(util.KVDBPath(d.Root), d.Key, options.ZSTD).WithReadOnly(true).WithBypassLockGuard(runtime.GOOS == "windows")
	db, err := badger.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestReadOnlyDatabaseSearchesWithoutWriting (SRC-7): a read-only database
// searches its index read-only, and one without an index gets
// ErrIndexNotReady and no index; before, a search built one.
func TestReadOnlyDatabaseSearchesWithoutWriting(t *testing.T) {
	d, _ := storeFixture(t, "fat/fat16", true)
	ro := reopenReadOnly(t, d)
	if _, err := fts.SearchCandidates(t.Context(), ro, []string{"txt"}, fts.QueryModeAnd, 0); err != nil {
		t.Fatal(err)
	}

	bare, _ := storeFixture(t, "fat/fat16", false)
	ro = reopenReadOnly(t, bare)
	if _, err := fts.SearchCandidates(t.Context(), ro, []string{"txt"}, fts.QueryModeAnd, 0); !errors.Is(err, fts.ErrIndexNotReady) {
		t.Fatalf("a search without an index: %v, want ErrIndexNotReady", err)
	}
	if _, err := os.Stat(util.FTSPath(bare.Root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a read-only search made an index: %v", err)
	}
}

// TestIndexFilesCountsUnreadableText (SRC-7): a file whose content cannot be
// read keeps its name document and is counted; before, the error was
// dropped and the file indexed as if it held no text.
func TestIndexFilesCountsUnreadableText(t *testing.T) {
	d, eid := storeFixture(t, "fat/fat32_fsagen", false)
	jobs := jobsOf(t, d.DB, eid)
	blobs, err := filepath.Glob(filepath.Join(d.Root, "blob", "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range blobs {
		if info, err := os.Stat(b); err == nil && info.Mode().IsRegular() {
			if err := os.Remove(b); err != nil {
				t.Fatal(err)
			}
		}
	}
	var list []fts.IndexedFileJob
	for _, j := range jobs {
		list = append(list, j)
	}
	stats, err := fts.IndexFiles(t.Context(), d.DB, list)
	var te *fts.TextError
	if !errors.As(err, &te) || te.Failed == 0 || stats.TextErrors != te.Failed || stats.Files != len(list) {
		t.Fatalf("index with every blob gone: %+v, %v", stats, err)
	}
	if _, ok := search(t, d.DB, fts.QueryModeAnd, "readme")[jobs["/docs/readme.txt"].FileID]; !ok {
		t.Fatal("a file whose text could not be read is not found by its name")
	}
}

// openElsewhere opens the index at root as another holder would, waiting
// briefly for its file lock.
func openElsewhere(root string) (bleve.Index, error) {
	return bleve.OpenUsing(util.FTSPath(root), map[string]any{"bolt_timeout": "200ms"})
}

// TestHeldIndexStaysOpen: an Index a run holds keeps the full-text index
// open across the run's evidence, and across the package's one-shot calls,
// which used to open and close it once each; Release closes it, once.
func TestHeldIndexStaysOpen(t *testing.T) {
	d, eid := storeFixture(t, "fat/fat16", true)
	var jobs []fts.IndexedFileJob
	for _, j := range jobsOf(t, d.DB, eid) {
		jobs = append(jobs, j)
	}
	x, err := fts.Acquire(d.DB, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.IndexFiles(t.Context(), jobs); err != nil {
		t.Fatal(err)
	}
	if _, err := fts.IndexFiles(t.Context(), d.DB, jobs); err != nil {
		t.Fatal(err)
	}
	if other, err := openElsewhere(d.Root); err == nil {
		other.Close()
		t.Fatal("the index was closed while a run held it")
	}
	if err := x.Release(); err != nil {
		t.Fatal(err)
	}
	if err := x.Release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
	other, err := openElsewhere(d.Root)
	if err != nil {
		t.Fatalf("the index is still open after Release: %v", err)
	}
	other.Close()
}
