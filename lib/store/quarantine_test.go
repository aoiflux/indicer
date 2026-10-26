package store

import (
	"encoding/base64"
	"os"
	"testing"

	"indicer/internal/testutil/fsfixture"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
)

// legacyIndexedEvidence stores the bytes of path as an evidence the way
// ingests before W6 did (no content model; container files stored as their
// raw bytes) and gives it one partition record, as their index pass did.
func legacyIndexedEvidence(t *testing.T, db *badger.DB, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := uuid.NewV7()
	eid := id[:]
	pid, _ := uuid.NewV7()
	rec := structs.NewEvidenceFile("legacy", 0, int64(len(content)), map[string]structs.InternalOffset{
		base64.StdEncoding.EncodeToString(pid[:]): {Start: 0, End: int64(len(content)) - 1},
	}, cnst.UnknownEvidenceType, "")
	rec.Completed, rec.IngestState = true, structs.IngestStateCompleted
	if err := dbio.CreateEvidence(db, eid, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := IngestChunks(t.Context(), ChunkPass{DB: db, ID: eid, Source: byteReaderAt(content), Size: int64(len(content)), Quiet: true}); err != nil {
		t.Fatal(err)
	}
	prec := structs.NewPartitionFile(base64.StdEncoding.EncodeToString(eid)+cnst.DataSeperator+"legacy_p0", 0, int64(len(content)), nil, "")
	if err := dbio.SetPartitionWithMembers(db, pid[:], prec, nil); err != nil {
		t.Fatal(err)
	}
	return eid
}

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	for _, m := range fsfixture.All(t) {
		if m.Name == name {
			return fsfixture.Open(t, m)
		}
	}
	t.Fatalf("no fixture %s", name)
	return ""
}

// TestQuarantineRawContainerIndexes (ING-7): an index an ingest before W6
// built over the raw bytes of an EWF or dynamic VHD/VHDX is marked invalid and
// hidden from the listing; an index over a raw image or a fixed VHD (a raw
// disk with a footer) is right and is kept.
func TestQuarantineRawContainerIndexes(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	util.SetChonkSize(4)
	if err := util.EnsureBlobPath(db.Root); err != nil {
		t.Fatal(err)
	}
	ewf := legacyIndexedEvidence(t, db.DB, fixturePath(t, "ewf/fat12_multiseg"))
	vhdx := legacyIndexedEvidence(t, db.DB, fixturePath(t, "vhd/fat16_vhdx"))
	dynamic := legacyIndexedEvidence(t, db.DB, fixturePath(t, "vhd/fat16_dynamic_vhd"))
	fixed := legacyIndexedEvidence(t, db.DB, fixturePath(t, "vhd/fat16_fixed_vhd"))
	raw := legacyIndexedEvidence(t, db.DB, fixturePath(t, "fat/fat12"))

	q, err := QuarantineRawContainerIndexes(t.Context(), db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 3 {
		t.Fatalf("quarantined %d indexes, want 3: %+v", len(q), q)
	}
	// What `dues list` shows: the listing the command prints.
	out, err := listOutput(db.DB, "all")
	if err != nil {
		t.Fatal(err)
	}
	listedByID := map[string]map[string]interface{}{}
	for _, e := range out["evidence"].([]map[string]interface{}) {
		listedByID[e["id"].(string)] = e
	}
	for _, c := range []struct {
		name    string
		id      []byte
		invalid bool
	}{{"ewf", ewf, true}, {"vhdx", vhdx, true}, {"dynamic vhd", dynamic, true}, {"fixed vhd", fixed, false}, {"raw", raw, false}} {
		e, err := dbio.GetEvidenceFile(c.id, db.DB)
		if err != nil {
			t.Fatal(err)
		}
		got := e.Index != nil && e.Index.State == structs.IndexInvalid
		if got != c.invalid {
			t.Errorf("%s: quarantined %v, want %v", c.name, got, c.invalid)
		}
		listed, found := listedByID[base64.StdEncoding.EncodeToString(c.id)]
		if !found {
			t.Fatalf("%s: not listed", c.name)
		}
		if n := listed["partitionCount"].(int); (n == 0) != c.invalid {
			t.Errorf("%s: listed with %d partitions", c.name, n)
		}
		if st, _ := listed["indexState"].(string); (st == string(structs.IndexInvalid)) != c.invalid {
			t.Errorf("%s: listed with index state %q", c.name, st)
		}
	}
	if again, err := QuarantineRawContainerIndexes(t.Context(), db.DB); err != nil || len(again) != 0 {
		t.Fatalf("a second run quarantined %d, %v", len(again), err)
	}
}
