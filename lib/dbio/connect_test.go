package dbio_test

import (
	"testing"

	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
)

// TestConnectDBLowResourceMode is the CFG-4 regression: --low set badger's
// NumCompactors to 1, which badger refuses, so no database could be opened.
// The low-resource setting is part of the tuning a database is opened with.
func TestConnectDBLowResourceMode(t *testing.T) {
	t.Parallel()
	for _, low := range []bool{true, false} {
		tun := cnst.DefaultTuning()
		tun.LowResource = low
		db, err := dbio.ConnectDBTuned(t.TempDir(), testdb.ZeroKey(), false, tun)
		if err != nil {
			t.Fatalf("ConnectDBTuned with LowResource=%v: %v", low, err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// ConnectDB opens with the default tuning.
	db, err := dbio.ConnectDB(t.TempDir(), testdb.ZeroKey(), false)
	if err != nil {
		t.Fatalf("ConnectDB: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}
