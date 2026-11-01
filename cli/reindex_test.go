package cli

import (
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/internal/testutil/testdb"
	"indicer/lib/dbio"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// TestReindexCommand: dues reindex rebuilds a named evidence, every evidence
// with --all, and with --failed only those whose index pass failed or was
// interrupted; repair --fix --reindex-failed does the same after recovery.
func TestReindexCommand(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	_, image := fixtureNamed(t, "fat/fat12")
	dbPath := filepath.Join(t.TempDir(), "db")
	opts := optsFor(dbPath, matrixChunkKB, nil)
	if err := StoreData(t.Context(), opts, image, false, false, false, false); err != nil {
		t.Fatal(err)
	}
	eid, rec := onlyEvidence(t, dbPath)
	id := base64.StdEncoding.EncodeToString(eid)

	var usage *lifecycle.UsageError
	if err := ReindexData(t.Context(), opts, ReindexOptions{}); !errors.As(err, &usage) {
		t.Fatalf("reindex with nothing selected: %v, want a usage error", err)
	}
	if err := ReindexData(t.Context(), opts, ReindexOptions{IDs: []string{"bm8tc3VjaA=="}}); !errors.As(err, &usage) {
		t.Fatalf("reindex of an unknown evidence: %v, want a usage error", err)
	}
	for _, ro := range []ReindexOptions{{IDs: []string{id}}, {IDs: []string{rec.FileHash}}, {All: true}} {
		if err := ReindexData(t.Context(), opts, ro); err != nil {
			t.Fatalf("reindex %+v: %v", ro, err)
		}
	}

	// An index pass the process did not finish is rebuilt by --failed.
	setIndex := func(state structs.IndexState) {
		h, err := openTestDB(dbPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		if err := dbio.UpdateEvidence(h.DB, eid, func(_ *badger.Txn, e *structs.EvidenceFile) error {
			e.Index.State = state
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	indexState := func() structs.IndexState {
		_, e := onlyEvidence(t, dbPath)
		return e.Index.State
	}
	setIndex(structs.IndexInterrupted)
	if err := ReindexData(t.Context(), opts, ReindexOptions{Failed: true}); err != nil || indexState() != structs.IndexComplete {
		t.Fatalf("reindex --failed: %v, state %s", err, indexState())
	}

	if err := RepairData(t.Context(), opts, RepairOptions{ReindexFailed: true}); !errors.As(err, &usage) {
		t.Fatalf("repair --reindex-failed without --fix: %v, want a usage error", err)
	}
	setIndex(structs.IndexRunning)
	doc, stderr, err := runRepair(t, opts, RepairOptions{Fix: true, ReindexFailed: true}, "", false, "reindex")
	if err != nil {
		t.Fatal(err)
	}
	if s := indexState(); s != structs.IndexComplete {
		t.Fatalf("after repair --fix --reindex-failed the index is %s", s)
	}
	var reindex struct {
		Evidence []struct {
			ID, Name, Outcome, Error string
		}
		Incomplete, Failed int
	}
	section(t, doc, "reindex", &reindex)
	if len(reindex.Evidence) != 1 || reindex.Evidence[0].ID != id || reindex.Evidence[0].Name != rec.Name ||
		reindex.Evidence[0].Outcome != "ok" || reindex.Evidence[0].Error != "" || reindex.Incomplete != 0 || reindex.Failed != 0 {
		t.Fatalf("reindex section: %s", doc["reindex"])
	}
	// The reindex report goes to stderr with the other steps' lines.
	if !strings.Contains(stderr, "\nReindexing ") {
		t.Fatalf("no reindex lines on stderr:\n%s", stderr)
	}
}
