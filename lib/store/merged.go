package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/zstdx"

	"github.com/dgraph-io/badger/v4"
)

// MergedTarget returns the ledger entry of the merged source evidence m of
// this size, and whether the target it names still holds that content: a
// COMPLETED evidence of the same size, content root and file hash the entry
// recorded.
func MergedTarget(db *badger.DB, m structs.MergeInfo, size int64) (structs.MergeLedgerEntry, bool, error) {
	var entry structs.MergeLedgerEntry
	var present bool
	err := db.View(func(txn *badger.Txn) error {
		err := dbio.GetRecordTxn(txn, dbio.MergeLedgerKey(m, size), &entry)
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("merge ledger of %x: %w", m.SourceEvidenceID, err)
		}
		e, err := dbio.GetEvidenceTxn(txn, entry.Target)
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("merge target %x: %w", entry.Target, err)
		}
		present = holdsContent(e) && e.Size == size && e.ContentRoot == entry.Root && e.FileHash == entry.FileHash
		return nil
	})
	return entry, present, err
}

// holdsContent reports whether e is COMPLETED and not a duplicate.
func holdsContent(e structs.EvidenceFile) bool {
	return (e.Completed || e.IngestState == structs.IngestStateCompleted) && e.IngestState != structs.IngestStateDuplicate
}

// SharedIDPresent reports whether target already holds, under the raw ID
// id, the content the source evidence src claims: a COMPLETED evidence of the
// same size, layout and content hash whose chunk at every offset is the
// source's. No chunk is read. It also returns where that evidence was merged
// from, nil for evidence stored here: its chunk hashes are in the package
// that merged it, so a package naming them shows nothing of the bytes.
func SharedIDPresent(ctx context.Context, target, source *badger.DB, id []byte, src structs.EvidenceFile) (*structs.MergeInfo, bool, error) {
	t, err := dbio.GetEvidenceFile(id, target)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("evidence %x: %w", id, err)
	}
	if !holdsContent(t) || t.Size != src.Size || t.ChunkSize != src.ChunkSize || t.FileHashAlgo != src.FileHashAlgo {
		return nil, false, nil
	}
	if !sameContentHash(t, src) {
		return nil, false, nil
	}
	same, err := sameRelations(ctx, target, source, id)
	if err != nil {
		return nil, false, fmt.Errorf("relations of evidence %x: %w", id, err)
	}
	return t.Merged, same, nil
}

// sameContentHash compares plain hashes when both records carry one, and
// otherwise file hashes, which mean the same only under the same strategy.
func sameContentHash(a, b structs.EvidenceFile) bool {
	if a.ContentModel == structs.ContentModelV2 && b.ContentModel == structs.ContentModelV2 {
		return a.PlainHash != "" && a.PlainHash == b.PlainHash
	}
	return a.FileHash != "" && a.FileHash == b.FileHash && a.HashStrategy == b.HashStrategy
}

// sameRelations reports whether a and b hold the same relations of id: the
// same offsets, each naming the same chunk.
func sameRelations(ctx context.Context, a, b *badger.DB, id []byte) (bool, error) {
	same := false
	err := a.View(func(ta *badger.Txn) error {
		return b.View(func(tb *badger.Txn) error {
			ia, ib := relationIterator(ta, id), relationIterator(tb, id)
			defer ia.Close()
			defer ib.Close()
			ia.Rewind()
			ib.Rewind()
			for ; ia.Valid() && ib.Valid(); ia.Next() {
				if err := ctx.Err(); err != nil {
					return err
				}
				if !bytes.Equal(ia.Item().Key(), ib.Item().Key()) {
					return nil
				}
				ca, err := relationChunk(ia.Item())
				if err != nil {
					return err
				}
				cb, err := relationChunk(ib.Item())
				if err != nil {
					return err
				}
				if !bytes.Equal(ca, cb) {
					return nil
				}
				ib.Next()
			}
			same = !ia.Valid() && !ib.Valid()
			return nil
		})
	})
	return same, err
}

func relationIterator(txn *badger.Txn, id []byte) *badger.Iterator {
	opts := badger.DefaultIteratorOptions
	opts.Prefix = dbio.RelationPrefix(id)
	opts.PrefetchValues = false
	return txn.NewIterator(opts)
}

// relationChunk is the chunk hash a relation names.
func relationChunk(item *badger.Item) ([]byte, error) {
	chash, err := item.ValueCopy(nil)
	if err != nil {
		return nil, err
	}
	// Relation values are zstd-compressed outside quick mode.
	if decoded, err := zstdx.DecodeAll(chash, nil); err == nil {
		chash = decoded
	}
	return chash, nil
}

// WalkEvidence calls fn with every evidence record of db, in key order, inside
// one read transaction. A record that does not decode reaches fn with its
// error; values that decode but are not evidence are skipped.
func WalkEvidence(db *badger.DB, fn func(eid []byte, e structs.EvidenceFile, err error) error) error {
	return db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(cnst.EviFileNamespace)
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			eid := dbio.EvidenceRawID(item.KeyCopy(nil))
			value, err := item.ValueCopy(nil)
			if err != nil {
				return fmt.Errorf("evidence %x: %w", eid, err)
			}
			e, ok, decodeErr := decodeEvidence(value)
			if decodeErr != nil {
				decodeErr = fmt.Errorf("decode evidence %x: %w", eid, decodeErr)
			} else if !ok {
				continue
			}
			if err := fn(eid, e, decodeErr); err != nil {
				return err
			}
		}
		return nil
	})
}

// CarryAliases records the aliases as of the merged source evidence m, of
// this size, as merged aliases of winner, each only where none is yet. It
// returns how many it wrote; carrying the same aliases again writes none.
func CarryAliases(db *badger.DB, winner []byte, m structs.MergeInfo, size int64, as []structs.AliasRecord) (int, error) {
	written := 0
	for start := 0; start < len(as); start += deleteBatch {
		batch := as[start:min(start+deleteBatch, len(as))]
		n := 0
		err := dbio.UpdateTxn(db, func(txn *badger.Txn) error {
			n = 0
			for _, a := range batch {
				key := dbio.MergeAliasKey(winner, m, size, a.Path)
				_, err := txn.Get(key)
				if err == nil {
					continue
				}
				if !errors.Is(err, badger.ErrKeyNotFound) {
					return err
				}
				merged := m
				// The source's attempt IDs name records the target never had.
				a.Merged, a.AttemptID = &merged, nil
				if err := dbio.PutRecordTxn(db, txn, key, a); err != nil {
					return err
				}
				n++
			}
			if n == 0 {
				return dbio.ErrNoChange
			}
			return nil
		})
		if err != nil {
			return written, fmt.Errorf("carry aliases of %x: %w", winner, err)
		}
		written += n
	}
	return written, nil
}
