package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"indicer/internal/faultinject"
	"indicer/lib/dbio"
	"indicer/lib/logging"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
)

type RecoveryReport struct {
	ScannedEvidence  int `json:"scannedEvidence"`
	IncompleteFound  int `json:"incompleteFound"`
	MarkedFailed     int `json:"markedFailed"`
	CompletedFlushed int `json:"completedFlushed"`
	// Aliased counts FLUSHED attempts whose content another evidence
	// committed first; their records were removed.
	Aliased int `json:"aliased"`
	// DuplicatesCleaned counts duplicate attempts whose removal finished.
	DuplicatesCleaned int `json:"duplicatesCleaned"`
	// IndexInterrupted counts evidence whose index pass the process did not
	// live to finish.
	IndexInterrupted int `json:"indexInterrupted"`
}

// Recovery actions, as RecoverEvidence reports them.
const (
	ActionMarkedFailed     = "marked_failed"
	ActionFlushedInvalid   = "flushed_invalid_marked_failed"
	ActionCompleted        = "auto_completed"
	ActionAliased          = "aliased"
	ActionDuplicateCleaned = "duplicate_cleaned"
	ActionIndexInterrupted = "index_interrupted"
)

// NeedsRecovery reports whether the evidence record e was left by an ingest
// that did not finish: an attempt still PENDING or FLUSHED, a duplicate
// whose records are not all removed, or an index pass still marked running.
func NeedsRecovery(e structs.EvidenceFile) bool {
	if e.IngestState == structs.IngestStateDuplicate {
		return true
	}
	if e.Index != nil && e.Index.State == structs.IndexRunning {
		return true
	}
	if e.Completed || e.Failed {
		return false
	}
	switch e.IngestState {
	case "", structs.IngestStatePending, structs.IngestStateFlushed:
		return true
	}
	return false
}

// RecoverEvidence brings one evidence record an unfinished ingest left to
// where the ingest would have taken it (W6), and returns what it did:
//
//   - PENDING: the chunk pass did not finish; the attempt is marked failed,
//     and storing the same file again resumes it;
//   - FLUSHED: every chunk is durable; the material is validated, then the
//     attempt is committed the way an ingest commits, so it becomes the
//     evidence, or an alias of the evidence that committed the same content
//     first, whose records are removed. Material known to be missing fails
//     it; a read error leaves it FLUSHED for the next writable open and is
//     returned (ING-10);
//   - a duplicate: its records are removed;
//   - an index pass marked running: it is marked interrupted.
//
// Every change is a read-modify-write of the current record, so a record
// another writer finished meanwhile is left alone.
func RecoverEvidence(ctx context.Context, db *badger.DB, id []byte, e structs.EvidenceFile) ([]string, error) {
	var actions []string
	if err := faultinject.Hit(faultinject.RecoveryMid); err != nil {
		return actions, err
	}
	switch {
	case e.IngestState == structs.IngestStateDuplicate:
		if err := CleanupDuplicate(ctx, db, id); err != nil {
			return actions, err
		}
		return append(actions, ActionDuplicateCleaned), nil

	case !e.Completed && !e.Failed && e.IngestState == structs.IngestStateFlushed:
		err := validateFlushed(ctx, id, e, db)
		if err != nil && ctx.Err() != nil {
			return actions, context.Cause(ctx)
		}
		if err != nil && !errors.Is(err, ErrMaterialMissing) {
			return actions, fmt.Errorf("validate flushed evidence %s: %w", CleanName(e.Name), err)
		}
		if err != nil {
			if err := failAttempt(db, id, "recovery: "+err.Error()); err != nil {
				return actions, err
			}
			return append(actions, ActionFlushedInvalid), nil
		}
		res, err := CommitEvidence(db, id)
		if err != nil {
			return actions, err
		}
		if res.Outcome == CommitAliased {
			if err := CleanupDuplicate(ctx, db, id); err != nil {
				return actions, err
			}
			return append(actions, ActionAliased), nil
		}
		actions = append(actions, ActionCompleted)

	case !e.Completed && !e.Failed:
		if err := failAttempt(db, id, "interrupted: the process ended before the ingest finished"); err != nil {
			return actions, err
		}
		return append(actions, ActionMarkedFailed), nil
	}
	if e.Index != nil && e.Index.State == structs.IndexRunning {
		changed, err := markIndexInterrupted(db, id)
		if err != nil {
			return actions, err
		}
		if changed {
			actions = append(actions, ActionIndexInterrupted)
		}
	}
	return actions, nil
}

// failAttempt marks an unfinished attempt failed, with why, and its index
// pass interrupted if it was running.
func failAttempt(db *badger.DB, id []byte, why string) error {
	err := dbio.UpdateEvidence(db, id, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		if e.Completed || e.Failed {
			return dbio.ErrNoChange
		}
		e.Failed = true
		if e.IngestState == "" {
			e.IngestState = structs.IngestStatePending
		}
		if e.LastError == "" {
			e.LastError = why
		}
		interruptIndex(e)
		return nil
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil
	}
	return err
}

func markIndexInterrupted(db *badger.DB, id []byte) (bool, error) {
	changed := false
	err := dbio.UpdateEvidence(db, id, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		changed = interruptIndex(e)
		if !changed {
			return dbio.ErrNoChange
		}
		return nil
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return false, nil
	}
	return changed, err
}

func interruptIndex(e *structs.EvidenceFile) bool {
	if e.Index == nil || e.Index.State != structs.IndexRunning {
		return false
	}
	e.Index.State = structs.IndexInterrupted
	e.Index.Reason = "the process ended while the index pass ran"
	e.Index.Finished = time.Now().UnixNano()
	return true
}

// RecoverIncompleteIngests recovers every evidence record an unfinished
// ingest left (RecoverEvidence), so retries and repair start from a known
// state. A read error leaves its record for the next writable open; the
// others are still recovered, and the errors are returned together. A
// cancelled ctx stops it between records, and the rest wait for the next
// writable open.
func RecoverIncompleteIngests(ctx context.Context, db *badger.DB) (RecoveryReport, error) {
	var report RecoveryReport

	type candidate struct {
		id []byte
		e  structs.EvidenceFile
	}
	var candidates []candidate
	err := db.View(func(txn *badger.Txn) error {
		return forEachEvidenceRecord(txn, func(id []byte, e structs.EvidenceFile) error {
			report.ScannedEvidence++
			if NeedsRecovery(e) {
				candidates = append(candidates, candidate{id: id, e: e})
			}
			return nil
		})
	})
	if err != nil {
		return report, err
	}

	var readErrs []error
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return report, context.Cause(ctx)
		}
		if !c.e.Completed && !c.e.Failed && c.e.IngestState != structs.IngestStateDuplicate {
			report.IncompleteFound++
		}
		actions, err := RecoverEvidence(ctx, db, c.id, c.e)
		for _, a := range actions {
			switch a {
			case ActionMarkedFailed, ActionFlushedInvalid:
				report.MarkedFailed++
			case ActionCompleted:
				report.CompletedFlushed++
			case ActionAliased:
				report.Aliased++
			case ActionDuplicateCleaned:
				report.DuplicatesCleaned++
			case ActionIndexInterrupted:
				report.IndexInterrupted++
			}
		}
		if len(actions) > 0 {
			logging.GetLogger().Info("RecoverIncompleteIngests RECOVERED",
				zap.String("file_name", CleanName(c.e.Name)),
				zap.String("actions", strings.Join(actions, ",")))
		}
		if err != nil {
			readErrs = append(readErrs, err)
		}
	}

	logging.GetLogger().Info("RecoverIncompleteIngests COMPLETE",
		zap.Int("scanned_evidence", report.ScannedEvidence),
		zap.Int("incomplete_found", report.IncompleteFound),
		zap.Int("marked_failed", report.MarkedFailed),
		zap.Int("completed_flushed", report.CompletedFlushed),
		zap.Int("aliased", report.Aliased),
		zap.Int("duplicates_cleaned", report.DuplicatesCleaned),
		zap.Int("index_interrupted", report.IndexInterrupted),
	)
	return report, errors.Join(readErrs...)
}
