package dbio

import (
	"errors"
	"math/rand/v2"
	"time"

	"indicer/internal/msgpackx"
	"indicer/lib/cnst"
	"indicer/lib/structs"
	"indicer/lib/zstdx"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

// ErrNoChange, returned by an UpdateEvidence callback, leaves the record as
// it was. UpdateEvidence then returns nil.
var ErrNoChange = errors.New("evidence record unchanged")

// ErrEvidenceExists is CreateEvidence finding a record already there.
var ErrEvidenceExists = errors.New("evidence record already exists")

// maxEvidenceUpdateAttempts bounds how often UpdateEvidence retries a
// transaction that conflicted with a concurrent write: up to about 1.5 s of
// pauses. 16 attempts (about 0.5 s) could starve one of eight busy writers on
// a loaded machine.
const maxEvidenceUpdateAttempts = 32

// UpdateEvidence changes the evidence record eid in one transaction: it reads
// the current record, passes it to fn, and writes back what fn leaves. Two
// writers therefore never overwrite each other's fields with a stale copy
// (ING-8). A transaction that conflicts with a concurrent write is retried
// from the read after a short random pause, so fn may run more than once and
// must change nothing but the record and txn. fn returning ErrNoChange skips
// the write; any other error aborts the update and is returned. A missing
// record is badger.ErrKeyNotFound.
func UpdateEvidence(db *badger.DB, eid []byte, fn func(txn *badger.Txn, e *structs.EvidenceFile) error) error {
	key := CanonicalEvidenceKey(eid)
	for attempt := 1; ; attempt++ {
		err := db.Update(func(txn *badger.Txn) error {
			e, err := getEvidenceTxn(txn, key)
			if err != nil {
				return err
			}
			if err := fn(txn, &e); err != nil {
				return err
			}
			return setEvidenceTxn(txn, key, e)
		})
		if errors.Is(err, ErrNoChange) {
			return nil
		}
		if !errors.Is(err, badger.ErrConflict) || attempt == maxEvidenceUpdateAttempts {
			return err
		}
		time.Sleep(conflictBackoff(attempt))
	}
}

// CreateEvidence writes a new evidence record, failing with
// ErrEvidenceExists rather than replacing one that is already there.
func CreateEvidence(db *badger.DB, eid []byte, e structs.EvidenceFile) error {
	key := CanonicalEvidenceKey(eid)
	return db.Update(func(txn *badger.Txn) error {
		_, err := txn.Get(key)
		if err == nil {
			return ErrEvidenceExists
		}
		if !errors.Is(err, badger.ErrKeyNotFound) {
			return err
		}
		return setEvidenceTxn(txn, key, e)
	})
}

// conflictBackoff is the pause before retry attempt+1: a random duration up
// to a ceiling that doubles per attempt from 1 ms to 64 ms, so writers that
// collided spread out.
func conflictBackoff(attempt int) time.Duration {
	ceiling := time.Millisecond << min(attempt-1, 6)
	return time.Duration(rand.Int64N(int64(ceiling))) + time.Millisecond/4
}

func getEvidenceTxn(txn *badger.Txn, key []byte) (structs.EvidenceFile, error) {
	var e structs.EvidenceFile
	item, err := txn.Get(key)
	if err != nil {
		return e, err
	}
	data, err := item.ValueCopy(nil)
	if err != nil {
		return e, err
	}
	if decoded, err := zstdx.DecodeAll(data, nil); err == nil {
		data = decoded
	}
	return e, msgpackx.Unmarshal(data, &e)
}

// setEvidenceTxn encodes the record the way SetFile does.
func setEvidenceTxn(txn *badger.Txn, key []byte, e structs.EvidenceFile) error {
	data, err := msgpack.Marshal(e)
	if err != nil {
		return err
	}
	if !cnst.QUICKOPT {
		data = cnst.ENCODER.EncodeAll(data, make([]byte, 0, len(data)))
	}
	return txn.Set(key, data)
}
