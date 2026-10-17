package dbio

import (
	"bytes"
	"fmt"

	"indicer/lib/cnst"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

// One key per member.
//
// Hash lookups (EH, PH, IH: content hash to the records holding it) were one
// msgpack list per hash, rewritten on every append, so a popular hash cost
// O(n) per append and concurrent appends conflicted (STO-22). A partition
// record carried all its indexed files in one map, which badger refuses past
// its value size limit, about a million files (ING-21). Both are now one key
// per member. Lists and maps written by earlier releases are still read, and
// members are reported once whichever form holds them.

func hashLookupMemberPrefix(lookupKey []byte) []byte {
	return util.AppendToBytesSlice(cnst.HashLookupMemberNamespace, lookupKey, cnst.DataSeperator)
}

func partitionMemberPrefix(pid []byte) []byte {
	return util.AppendToBytesSlice(cnst.PartitionMemberNamespace, CanonicalPartitionKey(pid), cnst.DataSeperator)
}

func GetHashLookupUUIDsByKey(lookupKey []byte, db *badger.DB) ([][]byte, error) {
	var ids [][]byte
	err := db.View(func(txn *badger.Txn) error {
		var err error
		ids, err = HashLookupMembersTxn(txn, lookupKey)
		return err
	})
	return ids, err
}

// HashLookupMembersTxn returns the members of the hash lookup lookupKey: a
// legacy list first, in its order, then members added since, in key order
// (for UUIDv7 record IDs, oldest first). It returns badger.ErrKeyNotFound
// when the lookup has no members.
func HashLookupMembersTxn(txn *badger.Txn, lookupKey []byte) ([][]byte, error) {
	var ids [][]byte
	seen := map[string]struct{}{}
	item, err := txn.Get(lookupKey)
	switch {
	case err == nil:
		var legacy [][]byte
		if err := item.Value(func(val []byte) error { return msgpack.Unmarshal(val, &legacy) }); err != nil {
			return nil, err
		}
		for _, id := range legacy {
			if _, dup := seen[string(id)]; !dup {
				seen[string(id)] = struct{}{}
				ids = append(ids, id)
			}
		}
	case err != badger.ErrKeyNotFound:
		return nil, err
	}

	prefix := hashLookupMemberPrefix(lookupKey)
	opts := badger.DefaultIteratorOptions
	opts.Prefix = prefix
	opts.PrefetchValues = false
	it := txn.NewIterator(opts)
	defer it.Close()
	for it.Rewind(); it.Valid(); it.Next() {
		id := bytes.Clone(it.Item().Key()[len(prefix):])
		if _, dup := seen[string(id)]; !dup {
			seen[string(id)] = struct{}{}
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, badger.ErrKeyNotFound
	}
	return ids, nil
}

func AppendHashLookupUUIDByKey(db *badger.DB, lookupKey []byte, uuid []byte) error {
	return db.Update(func(txn *badger.Txn) error {
		return AppendHashLookupUUIDByKeyTxn(txn, lookupKey, uuid)
	})
}

// AppendHashLookupUUIDByKeyTxn adds member to the hash lookup lookupKey. It
// writes one key and reads nothing, so it costs the same however many
// members the lookup has, and concurrent appends never conflict.
func AppendHashLookupUUIDByKeyTxn(txn *badger.Txn, lookupKey []byte, member []byte) error {
	return txn.Set(append(hashLookupMemberPrefix(lookupKey), member...), []byte{})
}

// SetPartitionWithMembers writes the partition record pfile under pid with
// its indexed files, members, as one key each (ING-21). The members are
// written first, so the record never counts members that are not there; the
// record itself carries only their number.
func SetPartitionWithMembers(db *badger.DB, pid []byte, pfile structs.PartitionFile, members map[string]structs.InternalOffset) error {
	batch := db.NewWriteBatch()
	defer batch.Cancel()
	prefix := partitionMemberPrefix(pid)
	for ihash, off := range members {
		val, err := msgpack.Marshal(off)
		if err != nil {
			return err
		}
		if err := batch.Set(util.AppendToBytesSlice(prefix, ihash), val); err != nil {
			return err
		}
	}
	if err := batch.Flush(); err != nil {
		return err
	}
	pfile.InternalObjects = nil
	pfile.MemberCount = len(members)
	return SetFile(CanonicalPartitionKey(pid), pfile, db)
}

// PartitionMemberCount returns how many indexed files the partition record
// pfile holds, in its legacy map or as member keys.
func PartitionMemberCount(pfile structs.PartitionFile) int {
	return len(pfile.InternalObjects) + pfile.MemberCount
}

// ForEachPartitionMember calls fn for every indexed file of the partition pid,
// whose record is pfile: the legacy map's entries, then the member keys.
func ForEachPartitionMember(db *badger.DB, pid []byte, pfile structs.PartitionFile, fn func(ihashB64 string, off structs.InternalOffset) error) error {
	if pfile.MemberCount == 0 {
		return forEachLegacyMember(pfile, fn)
	}
	return db.View(func(txn *badger.Txn) error {
		return ForEachPartitionMemberTxn(txn, pid, pfile, fn)
	})
}

// ForEachPartitionMemberTxn is ForEachPartitionMember inside txn.
func ForEachPartitionMemberTxn(txn *badger.Txn, pid []byte, pfile structs.PartitionFile, fn func(ihashB64 string, off structs.InternalOffset) error) error {
	if err := forEachLegacyMember(pfile, fn); err != nil || pfile.MemberCount == 0 {
		return err
	}
	prefix := partitionMemberPrefix(pid)
	opts := badger.DefaultIteratorOptions
	opts.Prefix = prefix
	it := txn.NewIterator(opts)
	defer it.Close()
	for it.Rewind(); it.Valid(); it.Next() {
		ihash := string(it.Item().Key()[len(prefix):])
		if _, dup := pfile.InternalObjects[ihash]; dup {
			continue
		}
		var off structs.InternalOffset
		if err := it.Item().Value(func(val []byte) error { return msgpack.Unmarshal(val, &off) }); err != nil {
			return fmt.Errorf("partition member %s: %w", ihash, err)
		}
		if err := fn(ihash, off); err != nil {
			return err
		}
	}
	return nil
}

func forEachLegacyMember(pfile structs.PartitionFile, fn func(ihashB64 string, off structs.InternalOffset) error) error {
	for ihash, off := range pfile.InternalObjects {
		if err := fn(ihash, off); err != nil {
			return err
		}
	}
	return nil
}

// PartitionMembers returns every indexed file of the partition pid as one
// map. Readers that can stream use ForEachPartitionMember instead.
func PartitionMembers(db *badger.DB, pid []byte, pfile structs.PartitionFile) (map[string]structs.InternalOffset, error) {
	members := make(map[string]structs.InternalOffset, PartitionMemberCount(pfile))
	err := ForEachPartitionMember(db, pid, pfile, func(ihash string, off structs.InternalOffset) error {
		members[ihash] = off
		return nil
	})
	return members, err
}
