package store

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"indicer/internal/faultinject"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
)

// Idempotent ingest (D1). An evidence's content identity is its content root,
// the hocho root over its chunk hashes. ER|||<root> names the one COMPLETED
// evidence holding that content; a later ingest of the same content becomes
// an alias of it (EA|||) and stores nothing of its own.

// CommitOutcome is how an ingest's commit ended.
type CommitOutcome string

const (
	// CommitStored: the evidence is COMPLETED and holds its content.
	CommitStored CommitOutcome = "stored"
	// CommitAliased: the content was already stored; the evidence is a
	// duplicate whose records are being removed, and an alias records it.
	CommitAliased CommitOutcome = "aliased"
)

// CommitResult says how a commit ended and which evidence holds the content.
type CommitResult struct {
	Outcome CommitOutcome
	Winner  []byte // raw ID
}

// CommitEvidence decides, in one transaction, what the FLUSHED evidence eid
// becomes (I2): a duplicate of a COMPLETED evidence holding the same content,
// or COMPLETED itself, entered under ER, EH and EQ. Two ingests of the same
// content committing at once conflict on the ER key, and the retry makes the
// second one the duplicate, so exactly one wins. Committing again returns the
// first outcome.
func CommitEvidence(db *badger.DB, eid []byte) (CommitResult, error) {
	var res CommitResult
	err := dbio.UpdateTxn(db, func(txn *badger.Txn) error {
		res = CommitResult{}
		e, err := dbio.GetEvidenceTxn(txn, eid)
		if err != nil {
			return err
		}
		switch {
		case e.Completed || e.IngestState == structs.IngestStateCompleted:
			res = CommitResult{Outcome: CommitStored, Winner: eid}
			return dbio.ErrNoChange
		case e.IngestState == structs.IngestStateDuplicate:
			res = CommitResult{Outcome: CommitAliased, Winner: e.DuplicateOf}
			return dbio.ErrNoChange
		case e.IngestState != structs.IngestStateFlushed:
			return fmt.Errorf("commit evidence %x: state %q, want %q", eid, e.IngestState, structs.IngestStateFlushed)
		}
		if err := clearAttemptTxn(txn, e, eid); err != nil {
			return err
		}

		winner, err := storedWinnerTxn(txn, eid, e.Size, e.ContentRoot, e.PlainHash)
		if err != nil {
			return err
		}
		if winner != nil {
			e.IngestState, e.DuplicateOf, e.Failed = structs.IngestStateDuplicate, winner, false
			if err := putAliasTxn(db, txn, winner, aliasOf(e, eid)); err != nil {
				return err
			}
			res = CommitResult{Outcome: CommitAliased, Winner: winner}
			return dbio.SetEvidenceTxn(db, txn, eid, e)
		}

		e.Completed, e.Failed, e.IngestState = true, false, structs.IngestStateCompleted
		if e.ContentRoot != "" {
			if err := txn.Set(dbio.RootLookupKey(e.ContentRoot), append([]byte(nil), eid...)); err != nil {
				return err
			}
		}
		member := dbio.CanonicalEvidenceKey(eid)
		if e.FileHash != "" {
			if err := dbio.AppendHashLookupUUIDByKeyTxn(txn, util.AppendToBytesSlice(cnst.EviFileHashLookupNamespace, e.FileHash), member); err != nil {
				return err
			}
		}
		if e.Fingerprint != "" {
			if err := dbio.AppendHashLookupUUIDByKeyTxn(txn, dbio.QuickLookupKey(e.Size, e.Fingerprint), member); err != nil {
				return err
			}
		}
		if err := faultinject.Hit(faultinject.IngestInCommit); err != nil {
			return err
		}
		res = CommitResult{Outcome: CommitStored, Winner: eid}
		return dbio.SetEvidenceTxn(db, txn, eid, e)
	})
	return res, err
}

// storedWinnerTxn returns the COMPLETED evidence, other than eid, that holds
// content of this size and root, or nil. Evidence stored before W6 has no ER
// key; its FileHash, under EH, is its plain hash or its hocho root, so both
// are tried.
func storedWinnerTxn(txn *badger.Txn, eid []byte, size int64, root, plain string) ([]byte, error) {
	if root != "" {
		item, err := txn.Get(dbio.RootLookupKey(root))
		switch {
		case err == nil:
			w, err := item.ValueCopy(nil)
			if err != nil {
				return nil, err
			}
			ok, err := completedEvidenceTxn(txn, w, eid, size)
			if err != nil || ok {
				return okWinner(w, ok), err
			}
		case !errors.Is(err, badger.ErrKeyNotFound):
			return nil, err
		}
	}
	for _, h := range []string{plain, root} {
		if h == "" {
			continue
		}
		members, err := dbio.HashLookupMembersTxn(txn, util.AppendToBytesSlice(cnst.EviFileHashLookupNamespace, h))
		if errors.Is(err, badger.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			w := dbio.EvidenceRawID(m)
			ok, err := completedEvidenceTxn(txn, w, eid, size)
			if err != nil {
				return nil, err
			}
			if ok {
				return append([]byte(nil), w...), nil
			}
		}
	}
	return nil, nil
}

func okWinner(w []byte, ok bool) []byte {
	if !ok {
		return nil
	}
	return w
}

// completedEvidenceTxn reports whether w, which is not self, is a COMPLETED
// evidence of the given size. A lookup naming a record that is gone or
// unreadable names no winner.
func completedEvidenceTxn(txn *badger.Txn, w, self []byte, size int64) (bool, error) {
	if bytes.Equal(w, self) {
		return false, nil
	}
	e, err := dbio.GetEvidenceTxn(txn, w)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	completed := e.Completed || e.IngestState == structs.IngestStateCompleted
	return completed && e.Size == size, nil
}

// aliasOf is the alias record of the attempt e (raw ID attempt).
func aliasOf(e structs.EvidenceFile, attempt []byte) structs.AliasRecord {
	a := structs.AliasRecord{Name: e.Name, Path: e.Name, Size: e.Size, AttemptID: append([]byte(nil), attempt...), At: time.Now().UnixNano()}
	if e.Source != nil {
		a.Source = *e.Source
		if len(e.Source.Members) > 0 {
			a.Path = e.Source.Members[0].Path
		}
	}
	return a
}

func putAliasTxn(db *badger.DB, txn *badger.Txn, winner []byte, a structs.AliasRecord) error {
	return dbio.PutRecordTxn(db, txn, dbio.AliasKey(winner, a.Path), a)
}

// WriteAlias records a, an ingest whose content winner already holds, found
// before anything was stored.
func WriteAlias(db *badger.DB, winner []byte, a structs.AliasRecord) error {
	if a.At == 0 {
		a.At = time.Now().UnixNano()
	}
	return dbio.UpdateTxn(db, func(txn *badger.Txn) error {
		return putAliasTxn(db, txn, winner, a)
	})
}

// Candidate is a COMPLETED evidence that may hold an ingest's content.
type Candidate struct {
	ID   []byte // raw
	Root string // its content root, base64; empty for legacy evidence
}

// StoredByIdentity returns the COMPLETED evidence holding content of this
// size, root and plain hash (base64), or nil.
func StoredByIdentity(db *badger.DB, size int64, root, plain string) ([]byte, error) {
	var w []byte
	err := db.View(func(txn *badger.Txn) error {
		var err error
		w, err = storedWinnerTxn(txn, nil, size, root, plain)
		return err
	})
	return w, err
}

// QuickCandidates returns the COMPLETED evidence of this size and fingerprint.
func QuickCandidates(db *badger.DB, size int64, fingerprint string) ([]Candidate, error) {
	var out []Candidate
	err := db.View(func(txn *badger.Txn) error {
		members, err := dbio.HashLookupMembersTxn(txn, dbio.QuickLookupKey(size, fingerprint))
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, m := range members {
			w := dbio.EvidenceRawID(m)
			e, err := dbio.GetEvidenceTxn(txn, w)
			if errors.Is(err, badger.ErrKeyNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if (e.Completed || e.IngestState == structs.IngestStateCompleted) && e.Size == size && e.Fingerprint == fingerprint {
				out = append(out, Candidate{ID: append([]byte(nil), w...), Root: e.ContentRoot})
			}
		}
		return nil
	})
	return out, err
}

// BeginAttempt creates the evidence record rec for an ingest of path, or,
// when an earlier ingest of the same path and content stopped before
// committing (FAILED or PENDING), resumes it under its ID: chunks it stored
// are not stored again. It returns the evidence's raw ID and whether it
// resumed.
func BeginAttempt(db *badger.DB, path string, rec structs.EvidenceFile) ([]byte, bool, error) {
	var eid []byte
	var resumed bool
	err := dbio.UpdateTxn(db, func(txn *badger.Txn) error {
		eid, resumed = nil, false
		key := dbio.AttemptKey(path)
		item, err := txn.Get(key)
		switch {
		case err == nil:
			prev, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}
			e, err := dbio.GetEvidenceTxn(txn, prev)
			if err != nil && !errors.Is(err, badger.ErrKeyNotFound) {
				return err
			}
			if err == nil && resumable(e, rec) {
				e.Attempt++
				e.Failed, e.LastError = false, ""
				e.IngestState = structs.IngestStatePending
				e.Name, e.Source, e.Features, e.Index = rec.Name, rec.Source, rec.Features, rec.Index
				eid, resumed = prev, true
				return dbio.SetEvidenceTxn(db, txn, eid, e)
			}
		case !errors.Is(err, badger.ErrKeyNotFound):
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		eid = id[:]
		rec.Attempt = 1
		if err := dbio.SetEvidenceTxn(db, txn, eid, rec); err != nil {
			return err
		}
		return txn.Set(key, append([]byte(nil), eid...))
	})
	return eid, resumed, err
}

// resumable reports whether the unfinished attempt e can carry on as rec: the
// same content (size and fingerprint) in the same layout, not yet committed.
func resumable(e, rec structs.EvidenceFile) bool {
	unfinished := !e.Completed && (e.IngestState == structs.IngestStatePending || e.IngestState == "" || e.Failed)
	return unfinished && e.IngestState != structs.IngestStateFlushed &&
		e.ContentModel == structs.ContentModelV2 &&
		e.Size == rec.Size && e.Fingerprint == rec.Fingerprint && e.ChunkSize == rec.ChunkSize
}

// clearAttemptTxn removes the attempt key of e's source when it names eid:
// a committed evidence is no longer resumed.
func clearAttemptTxn(txn *badger.Txn, e structs.EvidenceFile, eid []byte) error {
	if e.Source == nil || len(e.Source.Members) == 0 {
		return nil
	}
	key := dbio.AttemptKey(e.Source.Members[0].Path)
	item, err := txn.Get(key)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	cur, err := item.ValueCopy(nil)
	if err != nil {
		return err
	}
	if !bytes.Equal(cur, eid) {
		return nil
	}
	return txn.Delete(key)
}
