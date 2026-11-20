package dbio_test

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"

	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

// TestHashLookupAppendsConcurrently is the STO-22 regression. A hash lookup
// (EH, PH, IH) was one msgpack list rewritten on every append, so a popular
// hash (an empty file, a common DLL) cost O(n) per append and concurrent
// appends conflicted. Each member is now its own key.
func TestHashLookupAppendsConcurrently(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default)
	lookup := util.AppendToBytesSlice(cnst.IdxFileHashLookupNamespace, "aGFzaA==")
	const workers, each = 8, 250
	var wg sync.WaitGroup
	errs := make(chan error, workers*each)
	for w := range workers {
		wg.Go(func() {
			for i := range each {
				member := dbio.CanonicalIndexedKey([]byte(fmt.Sprintf("%08d%08d", w, i)))
				if err := dbio.AppendHashLookupUUIDByKey(db.DB, lookup, member); err != nil {
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append: %v", err)
	}
	ids, err := dbio.GetHashLookupUUIDsByKey(lookup, db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != workers*each {
		t.Fatalf("lookup holds %d members, want %d", len(ids), workers*each)
	}
	// Appending a member again changes nothing.
	if err := dbio.AppendHashLookupUUIDByKey(db.DB, lookup, ids[0]); err != nil {
		t.Fatal(err)
	}
	if again, _ := dbio.GetHashLookupUUIDsByKey(lookup, db.DB); len(again) != len(ids) {
		t.Fatalf("re-append gave %d members, want %d", len(again), len(ids))
	}
}

// TestHashLookupReadsLegacyList checks that a lookup written as one list by
// an earlier release is still read, together with members appended since.
func TestHashLookupReadsLegacyList(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default)
	lookup := util.AppendToBytesSlice(cnst.EviFileHashLookupNamespace, "bGVnYWN5")
	legacy := [][]byte{dbio.CanonicalEvidenceKey([]byte("0000000000000001")), dbio.CanonicalEvidenceKey([]byte("0000000000000002"))}
	raw, err := msgpack.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DB.Update(func(txn *badger.Txn) error { return txn.Set(lookup, raw) }); err != nil {
		t.Fatal(err)
	}
	newer := dbio.CanonicalEvidenceKey([]byte("0000000000000003"))
	if err := dbio.AppendHashLookupUUIDByKey(db.DB, lookup, newer); err != nil {
		t.Fatal(err)
	}
	if err := dbio.AppendHashLookupUUIDByKey(db.DB, lookup, legacy[1]); err != nil {
		t.Fatal(err)
	}
	ids, err := dbio.GetHashLookupUUIDsByKey(lookup, db.DB)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([][]byte{}, legacy...), newer)
	if len(ids) != len(want) {
		t.Fatalf("lookup holds %d members, want %d", len(ids), len(want))
	}
	for i := range want {
		if !bytes.Equal(ids[i], want[i]) {
			t.Fatalf("member %d is %q, want %q (legacy first, then newer, no duplicates)", i, ids[i], want[i])
		}
	}

	if _, err := dbio.GetHashLookupUUIDsByKey(util.AppendToBytesSlice(cnst.EviFileHashLookupNamespace, "bm9uZQ=="), db.DB); !errors.Is(err, badger.ErrKeyNotFound) {
		t.Fatalf("empty lookup: %v, want ErrKeyNotFound", err)
	}
}

// TestPartitionMembersOutsideRecord is the ING-21 regression. A partition
// record carried every indexed file in one msgpack map, and badger refuses a
// value larger than its value log file (64 MiB), so a volume with about a
// million files could not be indexed. Members are now one key each and the
// record stays the same size; records written with the map are still read.
func TestPartitionMembersOutsideRecord(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default)
	pid := []byte("partition-000001")
	members := map[string]structs.InternalOffset{}
	for i := range 20000 {
		members[fmt.Sprintf("aWR4-%06d", i)] = structs.InternalOffset{Start: int64(i) * 512, End: int64(i)*512 + 511}
	}
	if err := dbio.SetPartitionWithMembers(db.DB, pid, structs.NewPartitionFile("p", 0, 1<<30, nil, "aGFzaA=="), members); err != nil {
		t.Fatal(err)
	}
	raw, err := dbio.GetNode(dbio.CanonicalPartitionKey(pid), db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 1024 {
		t.Fatalf("partition record is %d bytes with 20000 members; it must not grow with them", len(raw))
	}

	pfile, err := dbio.GetPartitionFile(pid, db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if n := dbio.PartitionMemberCount(pfile); n != len(members) {
		t.Fatalf("member count %d, want %d", n, len(members))
	}
	seen := 0
	err = dbio.ForEachPartitionMember(db.DB, pid, pfile, func(ihash string, off structs.InternalOffset) error {
		if members[ihash] != off {
			return fmt.Errorf("member %s at %+v, want %+v", ihash, off, members[ihash])
		}
		seen++
		return nil
	})
	if err != nil || seen != len(members) {
		t.Fatalf("iterated %d members: %v", seen, err)
	}

	// A record written by an earlier release keeps its members in the map.
	legacyID := []byte("partition-000002")
	legacy := structs.NewPartitionFile("old", 0, 4096, map[string]structs.InternalOffset{"b2xk": {Start: 0, End: 9}}, "aGFzaA==")
	if err := dbio.SetFile(dbio.CanonicalPartitionKey(legacyID), legacy, db.DB); err != nil {
		t.Fatal(err)
	}
	pfile, err = dbio.GetPartitionFile(legacyID, db.DB)
	if err != nil {
		t.Fatal(err)
	}
	got, err := dbio.PartitionMembers(db.DB, legacyID, pfile)
	if err != nil || len(got) != 1 || got["b2xk"] != (structs.InternalOffset{Start: 0, End: 9}) || dbio.PartitionMemberCount(pfile) != 1 {
		t.Fatalf("legacy members %v, count %d: %v", got, dbio.PartitionMemberCount(pfile), err)
	}
}
