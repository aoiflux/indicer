package dbio_test

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/dbio"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// heapGrowth runs fn and returns how far the live heap rose above where it
// was when fn started.
func heapGrowth(fn func()) uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	base := m.HeapAlloc
	peak := testutil.PeakHeap(fn)
	if peak < base {
		return 0
	}
	return peak - base
}

// TestMillionPartitionMembers: a partition of a million files (the
// ubuntu.vhdx leaf indexes 1.5M) keeps a record of constant size, and its
// members are read back streaming, not gathered.
func TestMillionPartitionMembers(t *testing.T) {
	testutil.LongTest(t)
	db := testdb.Open(t, testdb.Default)
	const n = 1_000_000
	pid := []byte("partition-million")
	func() {
		members := make(map[string]structs.InternalOffset, n)
		for i := range n {
			members[fmt.Sprintf("aWR4LTEw-%07d", i)] = structs.InternalOffset{Start: int64(i) * 4096, End: int64(i)*4096 + 4095}
		}
		if err := dbio.SetPartitionWithMembers(db.DB, pid, structs.NewPartitionFile("big", 0, n*4096, nil, "aGFzaA=="), members); err != nil {
			t.Fatal(err)
		}
	}()
	pfile, err := dbio.GetPartitionFile(pid, db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if got := dbio.PartitionMemberCount(pfile); got != n {
		t.Fatalf("member count %d, want %d", got, n)
	}

	seen := 0
	var last int64 = -1
	growth := heapGrowth(func() {
		err = dbio.ForEachPartitionMember(db.DB, pid, pfile, func(_ string, off structs.InternalOffset) error {
			if off.End != off.Start+4095 {
				return fmt.Errorf("member at %+v", off)
			}
			last = max(last, off.Start)
			seen++
			return nil
		})
	})
	if err != nil || seen != n || last != (n-1)*4096 {
		t.Fatalf("iterated %d members (last at %d): %v", seen, last, err)
	}
	// badger's caches can take up to 96 MiB of this; gathering the members
	// would take several times the rest.
	if growth > 192<<20 {
		t.Fatalf("reading the members grew the heap by %d MiB", growth>>20)
	}
}

// TestPopularHashLookup: 200,000 objects of one content (empty files, a
// common DLL) each append one key to its hash lookup, and all are read back.
// The lookup used to be one value rewritten in full at every append (STO-22).
func TestPopularHashLookup(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default)
	const n = 200_000
	key := []byte("IH|||:empty-file-hash")
	member := func(i int) []byte {
		b := make([]byte, 16)
		binary.BigEndian.PutUint64(b[8:], uint64(i))
		return b
	}
	for i := 0; i < n; i += 1000 {
		err := db.DB.Update(func(txn *badger.Txn) error {
			for j := i; j < i+1000; j++ {
				if err := dbio.AppendHashLookupUUIDByKeyTxn(txn, key, member(j)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := dbio.GetHashLookupUUIDsByKey(key, db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("%d members, want %d", len(got), n)
	}
	seen := make(map[uint64]bool, n)
	for _, m := range got {
		seen[binary.BigEndian.Uint64(m[8:])] = true
	}
	if len(seen) != n {
		t.Fatalf("%d distinct members, want %d", len(seen), n)
	}
}
