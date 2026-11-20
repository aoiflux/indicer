//go:build faultinject

package ingest

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"indicer/internal/faultinject"
	"indicer/internal/testutil"
	"indicer/lib/store"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// injectOnce makes point fail the next merges, until the returned restore.
func injectOnce(t *testing.T, point string) func() {
	t.Helper()
	restore, err := faultinject.Set(point + "=error")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
	return restore
}

// TestMergeResumesStoredChunks: a merge that stops after its chunks are
// stored resumes under the same ID, and stores none of them again.
func TestMergeResumesStoredChunks(t *testing.T) {
	source, target := openDB(t), openDB(t)
	path := testutil.WriteRandomFile(t, t.TempDir(), "resume.bin", 50, 3*mergeChunk+11)
	sid := storeIn(t, source, Request{Path: path, NoIndex: true}).EvidenceID

	restore := injectOnce(t, faultinject.IngestBeforeFlushed)
	first, err := mergeInto(t, target, source, sid, mergePackage)
	if !errors.Is(err, faultinject.ErrInjected) || first.Bytes.New == 0 {
		t.Fatalf("first merge: %+v, %v", first, err)
	}
	restore()
	again, err := mergeInto(t, target, source, sid, mergePackage)
	if err != nil || again.Outcome != store.CommitStored || !again.Resumed || !bytes.Equal(again.Attempt, first.Attempt) || again.Bytes.New != 0 {
		t.Fatalf("rerun: %+v, %v; want a resume of %x storing nothing new", again, err, first.Attempt)
	}
}

// TestMergeResumesPendingSameID: a merge that stops right after its attempt
// began resumes it under the same ID.
func TestMergeResumesPendingSameID(t *testing.T) {
	source, target := openDB(t), openDB(t)
	path := testutil.WriteRandomFile(t, t.TempDir(), "pending.bin", 51, mergeChunk+11)
	sid := storeIn(t, source, Request{Path: path, NoIndex: true}).EvidenceID

	restore := injectOnce(t, faultinject.IngestAfterPending)
	first, err := mergeInto(t, target, source, sid, mergePackage)
	if !errors.Is(err, faultinject.ErrInjected) {
		t.Fatalf("first merge: %v", err)
	}
	restore()
	again, err := mergeInto(t, target, source, sid, mergePackage)
	if err != nil || !again.Resumed || !bytes.Equal(again.Attempt, first.Attempt) {
		t.Fatalf("rerun: %+v, %v; want a resume of %x", again, err, first.Attempt)
	}
}

// TestMergeRecoveredFlushedIsPresent: a merge that stopped before its commit
// is committed by recovery, ledger included, so the rerun finds it present;
// a stored digest mismatch recovery committed stays flagged.
func TestMergeRecoveredFlushedIsPresent(t *testing.T) {
	cases := []struct {
		name string
		edit func(e *structs.EvidenceFile)
		want string
	}{
		{"verified", nil, structs.EWFVerified},
		{"mismatch", func(e *structs.EvidenceFile) {
			md5 := slices.Clone(e.Source.EWF.StoredMD5)
			md5[0] ^= 0xff
			e.Source.EWF.StoredMD5 = md5
		}, structs.EWFMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source, target := openDB(t), openDB(t)
			sid := storeIn(t, source, Request{Path: fixture(t, "ewf/fat12_multiseg")}).EvidenceID
			if c.edit != nil {
				editEvidence(t, source, sid, c.edit)
			}
			restore := injectOnce(t, faultinject.IngestBeforeCommit)
			first, err := mergeInto(t, target, source, sid, mergePackage)
			if !errors.Is(err, faultinject.ErrInjected) {
				t.Fatalf("first merge: %v", err)
			}
			restore()
			if e := evidenceOf(t, target, first.Attempt); e.IngestState != structs.IngestStateFlushed {
				t.Fatalf("attempt state %s", e.IngestState)
			}
			recoverTarget(t, target)
			again, err := mergeInto(t, target, source, sid, mergePackage)
			if err != nil || again.Outcome != store.CommitPresent || !bytes.Equal(again.EvidenceID, first.Attempt) {
				t.Fatalf("rerun: %+v, %v", again, err)
			}
			if e := evidenceOf(t, target, again.EvidenceID); e.Source.EWF == nil || e.Source.EWF.Result != c.want {
				t.Fatalf("EWF %+v, want %s", e.Source.EWF, c.want)
			}
		})
	}
}

// recoverTarget runs the recovery a read-write open of db runs.
func recoverTarget(t *testing.T, db *badger.DB) {
	t.Helper()
	report, err := store.RecoverIncompleteIngests(t.Context(), db)
	if err != nil || report.CompletedFlushed != 1 {
		t.Fatalf("recovery: %+v, %v", report, err)
	}
}
