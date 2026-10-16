package dbio_test

import (
	"testing"

	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
)

// TestConnectDBLowResourceMode is the CFG-4 regression: --low set badger's
// NumCompactors to 1, which badger refuses, so no database could be opened.
func TestConnectDBLowResourceMode(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	for _, low := range []bool{true, false} {
		cnst.MEMOPT = low
		db, err := dbio.ConnectDB(t.TempDir(), testdb.ZeroKey())
		if err != nil {
			t.Fatalf("ConnectDB with MEMOPT=%v: %v", low, err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
