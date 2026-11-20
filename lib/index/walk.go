package index

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"indicer/lib/dbio"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// walkBatch is how many indexed records ForEachIndexed reads per transaction.
const walkBatch = 1024

// ForEachIndexed calls fn with the key (I|||…) and record of every indexed
// file of the evidence eid, partition by partition. fn runs outside any
// transaction, so it may read or write the database.
func ForEachIndexed(ctx context.Context, db *badger.DB, eid []byte, fn func(key []byte, rec structs.IndexedFile) error) error {
	e, err := dbio.GetEvidenceFile(eid, db)
	if err != nil {
		return err
	}
	for pidB64 := range e.InternalObjects {
		pid, err := base64.StdEncoding.DecodeString(pidB64)
		if err != nil {
			return fmt.Errorf("partition %q of evidence %x: %w", pidB64, eid, err)
		}
		prec, err := dbio.GetPartitionFile(pid, db)
		if errors.Is(err, badger.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		var ids []string
		err = dbio.ForEachPartitionMember(db, pid, prec, func(iid string, _ structs.InternalOffset) error {
			ids = append(ids, iid)
			return nil
		})
		if err != nil {
			return err
		}
		for i := 0; i < len(ids); i += walkBatch {
			if err := ctx.Err(); err != nil {
				return err
			}
			type item struct {
				key []byte
				rec structs.IndexedFile
			}
			var items []item
			err := db.View(func(txn *badger.Txn) error {
				for _, idB64 := range ids[i:min(i+walkBatch, len(ids))] {
					raw, err := base64.StdEncoding.DecodeString(idB64)
					if err != nil {
						continue
					}
					key := dbio.CanonicalIndexedKey(raw)
					var rec structs.IndexedFile
					switch err := dbio.GetRecordTxn(txn, key, &rec); {
					case errors.Is(err, badger.ErrKeyNotFound):
						continue
					case err != nil:
						return err
					}
					items = append(items, item{key, rec})
				}
				return nil
			})
			if err != nil {
				return err
			}
			for _, it := range items {
				if err := fn(it.key, it.rec); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
