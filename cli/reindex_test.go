package cli

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/internal/progress"
	"indicer/internal/testutil/testdb"
	"indicer/lib/dbio"
	"indicer/lib/ingest"
	"indicer/lib/structs"
	"indicer/lib/util"

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
	// Each evidence weighs its size on the overall bar.
	r := progress.New(t.Context(), nil)
	if err := ReindexData(progress.With(t.Context(), r), opts, ReindexOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	phases := r.Snapshot(0).Phases
	i := slices.IndexFunc(phases, func(v progress.View) bool { return v.Name == "evidence" })
	if want := rec.Size + itemCost; i < 0 || phases[i].Weight != want || phases[i].WeightDone != want {
		t.Fatalf("reindex phases %+v: want evidence weighing %d", phases, want)
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

// TestReindexJobsReadAsOneAtATime: a reindex of four evidence at a time, with
// the full-text index and the graph, prints what a reindex of one at a time
// prints, in the same order, and ends the same way for every evidence.
func TestReindexJobsReadAsOneAtATime(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	d := testdb.Open(t, testdb.Default)
	if err := util.EnsureBlobPath(d.Root); err != nil {
		t.Fatal(err)
	}
	sess := ingest.NewSession(d.DB)
	err := storeFolder(t.Context(), io.Discard, sess, folderForJobs(t), StoreOptions{Decode: true, Quiet: true, FTS: true, Enrich: true})
	if cerr := sess.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	reindex := func(jobs int) (string, reindexSummary, error) {
		var out bytes.Buffer
		sum, err := reindexEvidence(progress.Without(t.Context()), d.DB, ReindexOptions{All: true, FTS: true, Enrich: true, Jobs: jobs}, &out)
		var lines []string
		for _, line := range strings.Split(out.String(), "\n") {
			lines = append(lines, tookTime.ReplaceAllString(line, " in T"))
		}
		return strings.Join(lines, "\n"), sum, err
	}
	one, sumOne, errOne := reindex(1)
	four, sumFour, errFour := reindex(4)
	if one != four {
		t.Fatalf("jobs 4 printed\n%s\n\njobs 1 printed\n%s", four, one)
	}
	if fmt.Sprint(errOne) != fmt.Sprint(errFour) || !reflect.DeepEqual(sumOne, sumFour) {
		t.Fatalf("jobs 4 ended %+v, %v; jobs 1 %+v, %v", sumFour, errFour, sumOne, errOne)
	}
	// Ten evidence: the folder's twelve files hold two copies of a third.
	if len(sumOne.Evidence) != 10 || sumOne.Failed != 0 {
		t.Fatalf("reindex of the folder's evidence: %+v, %v", sumOne, errOne)
	}
}
