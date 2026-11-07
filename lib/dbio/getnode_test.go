package dbio_test

import (
	"bytes"
	"runtime"
	"testing"

	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
)

// TestGetNodeOwnsItsValue is the STO-18 regression. GetNode copied a value
// onto itself (item.ValueCopy(val) inside item.Value), so for a value badger
// keeps in its value log the result was badger's mapping of the log file, not
// a copy: it was written to, and it was gone once the database closed. The
// value here is past badger's value threshold, and the database is unencrypted
// so the log is read through its mapping.
func TestGetNodeOwnsItsValue(t *testing.T) {
	db := testdb.Open(t, testdb.Quick)
	key := []byte("S|||:large")
	want := bytes.Repeat([]byte("dues"), 1<<19) // 2 MiB
	if err := db.DB.Update(func(txn *badger.Txn) error { return txn.Set(key, want) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	opts := dbio.BadgerOptions(util.KVDBPath(db.Root), nil, options.Snappy, cnst.DefaultTuning()).
		WithReadOnly(true).WithBypassLockGuard(runtime.GOOS == "windows")
	ro, err := badger.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := dbio.GetNode(key, ro)
	if err != nil {
		_ = ro.Close()
		t.Fatal(err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("GetNode returned %d bytes that differ from the %d stored", len(got), len(want))
	}
}
