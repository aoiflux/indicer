package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"

	"indicer/internal/faultinject"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"indicer/lib/zstdx"

	"github.com/dgraph-io/badger/v4"
)

// Removing what an ingest wrote: the records of a duplicate attempt (D1),
// and an evidence's index before it is rebuilt. Every step deletes keys
// whose absence is the goal, so a run that stops part way is finished by
// running it again.

// deleteBatch is how many keys one delete transaction holds.
const deleteBatch = 4096

// CleanupDuplicate deletes everything the duplicate attempt eid wrote: its
// index records, its relations and reverse relations, and finally its
// record. The alias written at commit keeps the audit trail. A record that
// is already gone is done.
func CleanupDuplicate(ctx context.Context, db *badger.DB, eid []byte) error {
	e, err := dbio.GetEvidenceFile(eid, db)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if e.IngestState != structs.IngestStateDuplicate {
		return fmt.Errorf("cleanup of evidence %x: state %q is not a duplicate", eid, e.IngestState)
	}
	if err := DeleteEvidenceIndex(ctx, db, eid, nil); err != nil {
		return fmt.Errorf("cleanup of evidence %x: index: %w", eid, err)
	}
	if err := faultinject.Hit(faultinject.IngestDuplicateCleanup); err != nil {
		return err
	}
	if err := deleteObjectRelations(ctx, db, eid); err != nil {
		return fmt.Errorf("cleanup of evidence %x: relations: %w", eid, err)
	}
	ekey := dbio.CanonicalEvidenceKey(eid)
	return db.Update(func(txn *badger.Txn) error {
		if err := txn.Delete(dbio.FileSimhashKey(ekey)); err != nil {
			return err
		}
		return txn.Delete(ekey)
	})
}

// DeleteEvidenceIndex deletes every partition and indexed record of the
// evidence eid, with their members, lookups, reports and materialised
// objects, and empties the evidence's partition list. onIndexed, when not
// nil, is called with the key of each indexed record deleted, so a caller
// can remove what other stores hold for it (full-text documents).
func DeleteEvidenceIndex(ctx context.Context, db *badger.DB, eid []byte, onIndexed func(key []byte)) error {
	e, err := dbio.GetEvidenceFile(eid, db)
	if err != nil {
		return err
	}
	for pidB64 := range e.InternalObjects {
		pid, err := base64.StdEncoding.DecodeString(pidB64)
		if err != nil {
			return fmt.Errorf("partition %q of evidence %x: %w", pidB64, eid, err)
		}
		if err := deletePartition(ctx, db, pid, onIndexed); err != nil {
			return err
		}
	}
	return dbio.UpdateEvidence(db, eid, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		if len(e.InternalObjects) == 0 {
			return dbio.ErrNoChange
		}
		e.InternalObjects = nil
		return nil
	})
}

func deletePartition(ctx context.Context, db *badger.DB, pid []byte, onIndexed func([]byte)) error {
	pkey := dbio.CanonicalPartitionKey(pid)
	prec, err := dbio.GetPartitionFile(pkey, db)
	missing := errors.Is(err, badger.ErrKeyNotFound)
	if err != nil && !missing {
		return err
	}
	// Members are read from the keys themselves, not through the record's
	// count: an index pass that stopped part way wrote members its record
	// does not count yet.
	var members []string
	if !missing {
		for iid := range prec.InternalObjects {
			members = append(members, iid)
		}
	}
	prefix := dbio.PartitionMemberPrefix(pkey)
	err = db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			members = append(members, string(it.Item().Key()[len(prefix):]))
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := 0; i < len(members); i += deleteBatch {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := deleteIndexed(ctx, db, members[i:min(i+deleteBatch, len(members))], onIndexed); err != nil {
			return err
		}
	}
	if err := deletePrefix(ctx, db, prefix); err != nil {
		return err
	}
	return db.Update(func(txn *badger.Txn) error {
		if !missing && prec.FileHash != "" {
			lk := util.AppendToBytesSlice(cnst.PartiFileHashLookupNamespace, prec.FileHash)
			if err := txn.Delete(dbio.HashLookupMemberKey(lk, pkey)); err != nil {
				return err
			}
		}
		if err := txn.Delete(dbio.PartitionReportKey(pid)); err != nil {
			return err
		}
		return txn.Delete(pkey)
	})
}

// deleteIndexed deletes the indexed records named by base64 raw IDs. A member
// whose record is missing was left by a pass that stopped before writing it;
// the extent pages and the materialised object the pass writes first, keyed
// by the same ID, go too.
func deleteIndexed(ctx context.Context, db *badger.DB, ids []string, onIndexed func([]byte)) error {
	var objects, pages [][]byte
	var keys [][]byte
	err := db.View(func(txn *badger.Txn) error {
		for _, idB64 := range ids {
			raw, err := base64.StdEncoding.DecodeString(idB64)
			if err != nil {
				continue // not a record key this code wrote
			}
			ikey := dbio.CanonicalIndexedKey(raw)
			var rec structs.IndexedFile
			err = dbio.GetRecordTxn(txn, ikey, &rec)
			if errors.Is(err, badger.ErrKeyNotFound) {
				objects = append(objects, raw)
				pages = append(pages, raw)
				continue
			}
			if err != nil {
				return err
			}
			keys = append(keys, ikey, dbio.FileSimhashKey(ikey))
			if rec.FileHash != "" {
				lk := util.AppendToBytesSlice(cnst.IdxFileHashLookupNamespace, rec.FileHash)
				keys = append(keys, dbio.HashLookupMemberKey(lk, ikey))
			}
			if c := rec.Content; c != nil {
				if c.ExtentPages > 0 {
					pages = append(pages, raw)
				}
				if len(c.ObjectID) > 0 {
					objects = append(objects, c.ObjectID)
				}
			}
			if onIndexed != nil {
				onIndexed(ikey)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, oid := range objects {
		if err := DeleteObject(ctx, db, oid); err != nil {
			return err
		}
	}
	for _, iid := range pages {
		if err := deletePrefix(ctx, db, dbio.ExtentPagePrefix(iid)); err != nil {
			return err
		}
	}
	return deleteKeys(db, keys)
}

// DeleteObject deletes a materialised object: its relations, then its record.
// An object that was never finished (a decode failed part way) is deleted the
// same way.
func DeleteObject(ctx context.Context, db *badger.DB, oid []byte) error {
	if err := deleteObjectRelations(ctx, db, oid); err != nil {
		return err
	}
	return db.Update(func(txn *badger.Txn) error { return txn.Delete(dbio.ObjectKey(oid)) })
}

// deleteObjectRelations deletes every relation of the object rid and the
// reverse relation each one implies. Chunks stay: other objects may share
// them, and a chunk with no relation costs space, never correctness.
func deleteObjectRelations(ctx context.Context, db *badger.DB, rid []byte) error {
	prefix := dbio.RelationPrefix(rid)
	for {
		var keys [][]byte
		err := db.View(func(txn *badger.Txn) error {
			opts := badger.DefaultIteratorOptions
			opts.Prefix = prefix
			it := txn.NewIterator(opts)
			defer it.Close()
			for it.Rewind(); it.Valid() && len(keys) < deleteBatch; it.Next() {
				key := it.Item().KeyCopy(nil)
				keys = append(keys, key)
				off, err := strconv.ParseInt(string(key[len(prefix):]), 10, 64)
				if err != nil {
					continue
				}
				chash, err := it.Item().ValueCopy(nil)
				if err != nil {
					return err
				}
				if decoded, err := zstdx.DecodeAll(chash, nil); err == nil {
					chash = decoded
				}
				keys = append(keys, dbio.ReverseRelationAppendMemberKey(chash, off, rid))
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			return nil
		}
		if err := deleteKeys(db, keys); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// deletePrefix deletes every key under prefix.
func deletePrefix(ctx context.Context, db *badger.DB, prefix []byte) error {
	for {
		var keys [][]byte
		err := db.View(func(txn *badger.Txn) error {
			opts := badger.DefaultIteratorOptions
			opts.Prefix = prefix
			opts.PrefetchValues = false
			it := txn.NewIterator(opts)
			defer it.Close()
			for it.Rewind(); it.Valid() && len(keys) < deleteBatch; it.Next() {
				keys = append(keys, it.Item().KeyCopy(nil))
			}
			return nil
		})
		if err != nil || len(keys) == 0 {
			return err
		}
		if err := deleteKeys(db, keys); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func deleteKeys(db *badger.DB, keys [][]byte) error {
	if len(keys) == 0 {
		return nil
	}
	wb := db.NewWriteBatch()
	defer wb.Cancel()
	for _, k := range keys {
		if err := wb.Delete(k); err != nil {
			return err
		}
	}
	return wb.Flush()
}
