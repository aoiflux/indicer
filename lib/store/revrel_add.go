package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"indicer/internal/progress"
	"slices"
	"strconv"

	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/zstdx"

	"github.com/dgraph-io/badger/v4"
)

// Features an evidence records it was stored with.
const (
	FeatureRevrel  = "revrel"
	FeatureSimhash = "simhash"
)

// HasReverseRelations reports whether the chunks of the stored evidence eid
// have reverse relations, which near needs to find where else they occur.
// An evidence stored since W6 says so in its features; for an earlier one,
// the reverse relation of its first chunk is looked for. An empty evidence
// has no chunks to look for.
func HasReverseRelations(db *badger.DB, eid []byte, e structs.EvidenceFile) (bool, error) {
	return hasFeature(db, eid, e, FeatureRevrel, func(rid, chash []byte) (bool, error) {
		return dbio.ChunkOccursAt(db, chash, 0, rid)
	})
}

// HasSignatures reports whether the chunks of the stored evidence eid have
// SimHash signatures, which near's deep mode compares.
func HasSignatures(db *badger.DB, eid []byte, e structs.EvidenceFile) (bool, error) {
	return hasFeature(db, eid, e, FeatureSimhash, func(_, chash []byte) (bool, error) {
		_, err := dbio.GetChonkSignature(chash, db)
		if errors.Is(err, badger.ErrKeyNotFound) {
			return false, nil
		}
		return err == nil, err
	})
}

func hasFeature(db *badger.DB, eid []byte, e structs.EvidenceFile, feature string, probe func(rid, chash []byte) (bool, error)) (bool, error) {
	if slices.Contains(e.Features, feature) || e.Size == 0 {
		return true, nil
	}
	if e.ContentModel == structs.ContentModelV2 {
		return false, nil
	}
	rid := dbio.EvidenceRawID(eid)
	chash, err := dbio.RelationAt(db, rid, 0)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return probe(rid, chash)
}

// AddReverseRelations records, for the stored evidence eid, where each of its
// chunks and of its materialised objects' chunks occurs, from their
// relations, and adds the feature to the evidence: what near needs, for
// evidence stored without --revrel (SRC-12). Storing the same file again does
// not add them, since it only records an alias (D1). It is idempotent, and
// returns the reverse relations written.
func AddReverseRelations(ctx context.Context, db *badger.DB, eid []byte) (int64, error) {
	rid := dbio.EvidenceRawID(eid)
	e, err := dbio.GetEvidenceFile(eid, db)
	if err != nil {
		return 0, err
	}
	if !e.Completed {
		return 0, fmt.Errorf("evidence %x is not stored", rid)
	}
	owners := [][]byte{rid}
	objects, err := materialisedObjects(ctx, db, e)
	if err != nil {
		return 0, err
	}
	owners = append(owners, objects...)

	var n int64
	for _, owner := range owners {
		m, err := addOwnerReverseRelations(ctx, db, owner)
		n += m
		if err != nil {
			return n, err
		}
	}
	err = dbio.UpdateEvidence(db, eid, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		if slices.Contains(e.Features, FeatureRevrel) {
			return dbio.ErrNoChange
		}
		e.Features = append(e.Features, FeatureRevrel)
		return nil
	})
	return n, err
}

// addOwnerReverseRelations writes a reverse relation for every relation of
// the object owner (an evidence or a materialised object).
func addOwnerReverseRelations(ctx context.Context, db *badger.DB, owner []byte) (int64, error) {
	batch := db.NewWriteBatch()
	defer batch.Cancel()
	prefix := dbio.RelationPrefix(owner)
	var n int64
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			if n%4096 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			off, err := strconv.ParseInt(string(it.Item().Key()[len(prefix):]), 10, 64)
			if err != nil {
				return fmt.Errorf("relation key %q: %w", it.Item().Key(), err)
			}
			chash, err := it.Item().ValueCopy(nil)
			if err != nil {
				return err
			}
			if decoded, err := zstdx.DecodeAll(chash, nil); err == nil {
				chash = decoded
			}
			if err := dbio.SetBatchNode(db, dbio.ReverseRelationAppendMemberKey(chash, off, owner), owner, batch); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return n, err
	}
	return n, batch.Flush()
}

// materialisedObjects returns the IDs of the objects an evidence's index
// materialised.
func materialisedObjects(ctx context.Context, db *badger.DB, e structs.EvidenceFile) ([][]byte, error) {
	var out [][]byte
	for pidB64 := range e.InternalObjects {
		pid, err := base64.StdEncoding.DecodeString(pidB64)
		if err != nil {
			return nil, fmt.Errorf("partition ID %q: %w", pidB64, err)
		}
		prec, err := dbio.GetPartitionFile(pid, db)
		if errors.Is(err, badger.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		err = dbio.ForEachPartitionMember(db, pid, prec, func(iidB64 string, off structs.InternalOffset) error {
			if off.End >= off.Start {
				return nil // an evidence range: its chunks are the evidence's
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			iid, err := base64.StdEncoding.DecodeString(iidB64)
			if err != nil {
				return nil
			}
			rec, err := dbio.GetIndexedFile(iid, db)
			if errors.Is(err, badger.ErrKeyNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if rec.Content != nil && rec.Content.Kind == structs.ContentMaterialized {
				out = append(out, rec.Content.ObjectID)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// AddMissingReverseRelations adds reverse relations to every stored evidence
// stored without them. It returns how many evidence lacked them and the
// reverse relations written.
func AddMissingReverseRelations(ctx context.Context, db *badger.DB) (int, int64, error) {
	var ids [][]byte
	err := db.View(func(txn *badger.Txn) error {
		return forEachEvidenceRecord(txn, func(key []byte, e structs.EvidenceFile) error {
			if !e.Completed || e.IngestState == structs.IngestStateDuplicate {
				return nil
			}
			ok, err := HasReverseRelations(db, key, e)
			if err == nil && !ok {
				ids = append(ids, append([]byte(nil), key...))
			}
			return err
		})
	})
	if err != nil {
		return 0, 0, err
	}
	var total int64
	ph := progress.Start(ctx, "revrel", progress.Evidence, int64(len(ids)))
	relations := ph.Meter("relations", progress.Records)
	for _, id := range ids {
		n, err := AddReverseRelations(ctx, db, id)
		total += n
		relations.Add(n)
		if err != nil {
			err = fmt.Errorf("add reverse relations to evidence %x: %w", dbio.EvidenceRawID(id), err)
			ph.Done(err)
			return len(ids), total, err
		}
		ph.Add(1)
	}
	ph.Done(nil)
	return len(ids), total, nil
}
