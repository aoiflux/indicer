package store

import (
	"encoding/base64"
	"fmt"
	"testing"

	"indicer/internal/testutil/testdb"
	"indicer/lib/dbio"
	"indicer/lib/structs"
)

// TestListScansForLegacyPartitionsOnce: a listing finds the partitions of
// evidence whose record does not link them by scanning the database. It did
// so once per such evidence, and every evidence stored without partitions (a
// file, or anything stored with --no-index) is one, so listing 1,500 files
// took 40 s and 9,000 more than five minutes. Records written since W6 link
// every partition and are not scanned for; older ones share one scan.
func TestListScansForLegacyPartitionsOnce(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	put := func(i int, v2 bool) []byte {
		id := []byte(fmt.Sprintf("evidence-%07d", i))
		rec := structs.NewEvidenceFile(fmt.Sprintf("f%d.bin", i), 0, 100, nil, "bin", fmt.Sprintf("hash-%d", i))
		rec.Completed, rec.IngestState = true, structs.IngestStateCompleted
		if v2 {
			rec.ContentModel = structs.ContentModelV2
		}
		if err := dbio.SetFile(id, rec, db.DB); err != nil {
			t.Fatal(err)
		}
		return id
	}
	for i := range 30 {
		put(i, true)
	}
	var legacy [][]byte
	for i := 30; i < 33; i++ {
		legacy = append(legacy, put(i, false))
	}
	// The oldest layout names a partition after its evidence.
	pname := base64.StdEncoding.EncodeToString(legacy[1]) + "|||p0"
	if err := dbio.SetFile(dbio.CanonicalPartitionKey([]byte("partition-legacy")), structs.NewPartitionFile(pname, 0, 100, nil, "phash"), db.DB); err != nil {
		t.Fatal(err)
	}

	before := partitionScans.Load()
	out, err := listOutput(db.DB, "all")
	if err != nil {
		t.Fatal(err)
	}
	if n := partitionScans.Load() - before; n > 1 {
		t.Fatalf("the listing scanned the database %d times for partitions, want at most once", n)
	}
	found := false
	for _, e := range out["evidence"].([]map[string]interface{}) {
		if e["name"] == "f31.bin" {
			found = e["partitionCount"] == 1
		}
	}
	if !found {
		t.Fatal("the legacy evidence's partition, found by name, is not listed")
	}
}
