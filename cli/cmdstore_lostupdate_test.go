package cli

import (
	"testing"
	"time"

	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
	"github.com/edsrzf/mmap-go"
)

// TestCompletionKeepsIndexWritesMadeWhileAwaitingHash is the ING-8
// regression. The COMPLETED transition read the evidence record, waited for
// the asynchronous evidence hash, then wrote back the copy it had read, so
// the partition map the indexer persisted during the wait was lost.
func TestCompletionKeepsIndexWritesMadeWhileAwaitingHash(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	cnst.StoreHashStrategy = cnst.AsyncHashStrategy

	db, err := badger.Open(badger.DefaultOptions(t.TempDir()).WithLogger(nil))
	if err != nil {
		t.Fatalf("badger.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	var mapped mmap.MMap
	infile := structs.NewInputFile(db, nil, mapped, "sample.dd", cnst.EviFileNamespace, nil, 8192, 0)
	evidence := structs.NewEvidenceFile("sample.dd", 0, 8192, nil, "raw", "")
	evidence.IngestState = structs.IngestStateFlushed
	if err := dbio.SetFile(infile.GetID(), evidence, db); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}

	hashCh := make(chan asyncFileHashResult)
	done := make(chan error, 1)
	go func() {
		done <- transitionEvidenceFileToCompleted(infile, "sample.dd", db, nil, evidenceHashOutcome{resultCh: hashCh})
	}()

	// Let the transition reach its wait for the hash, then index meanwhile,
	// as the asynchronous indexer does.
	time.Sleep(100 * time.Millisecond)
	objects := map[string]structs.InternalOffset{
		"partition-0": {Start: 0, End: 4095},
		"partition-1": {Start: 4096, End: 8191},
	}
	if err := persistEvidenceInternalObjects(infile.GetID(), objects, db); err != nil {
		t.Fatalf("persist partitions: %v", err)
	}
	hashCh <- asyncFileHashResult{encodedHash: "c2FtcGxlLWhhc2g="}
	if err := <-done; err != nil {
		t.Fatalf("transition: %v", err)
	}

	got, err := dbio.GetEvidenceFile(infile.GetID(), db)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Completed || got.IngestState != structs.IngestStateCompleted || got.FileHash != "c2FtcGxlLWhhc2g=" {
		t.Fatalf("not completed: completed=%v state=%q hash=%q", got.Completed, got.IngestState, got.FileHash)
	}
	if len(got.InternalObjects) != len(objects) {
		t.Fatalf("completion lost the partitions indexed while it waited: have %d, want %d", len(got.InternalObjects), len(objects))
	}
}
