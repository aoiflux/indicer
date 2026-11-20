package dbio_test

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"testing"

	"indicer/internal/testutil/testdb"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

const (
	mergeTestSize = 4096
	mergeTestPath = "/case/disk.E01"
)

func mergeTestInfo() structs.MergeInfo {
	return structs.MergeInfo{
		Package:          "aa11",
		SourceEvidenceID: bytes.Repeat([]byte{0x42}, 16),
		SourceFileHash:   "c291cmNlIGhhc2g=",
		SourceHashAlgo:   dbmeta.FileHashBLAKE3,
	}
}

func TestMergeLedgerKeyQualifiedByContent(t *testing.T) {
	t.Parallel()
	m := mergeTestInfo()
	key := dbio.MergeLedgerKey(m, mergeTestSize)
	if !bytes.Equal(key, dbio.MergeLedgerKey(mergeTestInfo(), mergeTestSize)) {
		t.Fatal("equal inputs gave different ledger keys")
	}
	// Only the source and its content name the entry, not the package.
	other := m
	other.Package, other.At = "bb22", 1
	if !bytes.Equal(key, dbio.MergeLedgerKey(other, mergeTestSize)) {
		t.Error("the package changed the ledger key")
	}
	hash, algo := m, m
	hash.SourceFileHash = "b3RoZXIgaGFzaA=="
	algo.SourceHashAlgo = dbmeta.FileHashSHA3
	for what, k := range map[string][]byte{
		"hash": dbio.MergeLedgerKey(hash, mergeTestSize),
		"algo": dbio.MergeLedgerKey(algo, mergeTestSize),
		"size": dbio.MergeLedgerKey(m, mergeTestSize+1),
	} {
		if bytes.Equal(k, key) {
			t.Errorf("the same ID with another %s gave the same ledger key", what)
		}
	}
}

func TestMergeAttemptKeyIsNotAPathKey(t *testing.T) {
	t.Parallel()
	m := mergeTestInfo()
	key := dbio.MergeAttemptKey(m, mergeTestSize)
	ledger := dbio.MergeLedgerKey(m, mergeTestSize)
	for _, p := range []string{
		string(ledger),
		hex.EncodeToString(ledger),
		mergeTestPath,
		`C:\case\disk.E01`,
		filepath.Join(t.TempDir(), "disk.E01"),
	} {
		if bytes.Equal(key, dbio.AttemptKey(p)) {
			t.Errorf("the merge attempt key equals AttemptKey(%q)", p)
		}
	}
	if len(key) != len(dbio.AttemptKey(mergeTestPath)) {
		t.Errorf("merge attempt key is %d bytes, an attempt key %d", len(key), len(dbio.AttemptKey(mergeTestPath)))
	}
}

func TestMergeAliasKeyNeverLocal(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default).DB
	winner := bytes.Repeat([]byte{0x07}, 16)
	m := mergeTestInfo()
	merged := dbio.MergeAliasKey(winner, m, mergeTestSize, mergeTestPath)
	local := dbio.AliasKey(winner, mergeTestPath)
	if bytes.Equal(merged, local) {
		t.Fatal("a merged alias key equals the local alias key at the same path")
	}
	if len(merged) != len(local) || !bytes.HasPrefix(merged, dbio.AliasPrefix(winner)) {
		t.Fatalf("merged alias key %x does not have the shape of an alias key of %x", merged, winner)
	}
	err := db.Update(func(txn *badger.Txn) error {
		if err := dbio.PutRecordTxn(db, txn, local, structs.AliasRecord{Path: mergeTestPath}); err != nil {
			return err
		}
		return dbio.PutRecordTxn(db, txn, merged, structs.AliasRecord{Path: mergeTestPath, Merged: &m})
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := dbio.Aliases(db, winner)
	if err != nil {
		t.Fatal(err)
	}
	var withMerged int
	for _, a := range got {
		if a.Merged != nil {
			withMerged++
		}
	}
	if len(got) != 2 || withMerged != 1 {
		t.Fatalf("Aliases = %+v, want the local and the merged alias", got)
	}
}

// TestAliasesExactLength: a 32-byte legacy ID that starts with a 16-byte ID
// shares its alias prefix.
func TestAliasesExactLength(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default).DB
	short := bytes.Repeat([]byte{0x11}, 16)
	long := append(append([]byte(nil), short...), bytes.Repeat([]byte{0x22}, 16)...)
	const shortPath, longPath = "/case/short.bin", "/case/long.bin"
	err := db.Update(func(txn *badger.Txn) error {
		if err := dbio.PutRecordTxn(db, txn, dbio.AliasKey(short, shortPath), structs.AliasRecord{Path: shortPath}); err != nil {
			return err
		}
		return dbio.PutRecordTxn(db, txn, dbio.AliasKey(long, longPath), structs.AliasRecord{Path: longPath})
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := dbio.Aliases(db, short)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != shortPath {
		t.Fatalf("Aliases of the 16-byte ID = %+v, want only %s", got, shortPath)
	}
	got, err = dbio.Aliases(db, long)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != longPath {
		t.Fatalf("Aliases of the 32-byte ID = %+v, want only %s", got, longPath)
	}
}
