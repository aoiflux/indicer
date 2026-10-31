package store

import (
	"context"
	"encoding/base64"
	"errors"

	"fmt"
	"indicer/internal/progress"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// VerifyResult is the outcome of verifying one evidence.
type VerifyResult struct {
	ID      string // base64 evidence ID
	Name    string
	Size    int64
	Problem string // empty when the evidence verified
}

// VerifyReport is what VerifyEvidence found.
type VerifyReport struct {
	Results            []VerifyResult
	Verified, Problems int
}

// VerifyEvidence reads back every stored chunk of every completed or flushed
// evidence and checks it against its hash (ValidateContent), the check
// behind `dues repair --verify`. Evidence that is missing material or holds
// corrupt chunks is reported with its first problem, and the run goes on.
// An error is returned only when the database cannot be read or ctx ends.
func VerifyEvidence(ctx context.Context, db *badger.DB) (report VerifyReport, err error) {
	type evidence struct {
		id  []byte
		rec structs.EvidenceFile
	}
	var all []evidence
	err = db.View(func(txn *badger.Txn) error {
		return forEachEvidenceRecord(txn, func(rawID []byte, ev structs.EvidenceFile) error {
			if ev.Failed || !(ev.Completed || ev.IngestState == structs.IngestStateFlushed) {
				return nil
			}
			all = append(all, evidence{rawID, ev})
			return nil
		})
	})
	if err != nil {
		return VerifyReport{}, err
	}

	var size int64
	for _, e := range all {
		size += e.rec.Size
	}
	ph := progress.Start(ctx, "verify", progress.Bytes, size)
	defer func() {
		if report.Problems > 0 {
			ph.Warn(fmt.Sprintf("%d damaged", report.Problems))
		}
		ph.Done(err)
	}()
	for _, e := range all {
		r := VerifyResult{
			ID:   base64.StdEncoding.EncodeToString(e.id),
			Name: firstCleanName(e.rec.Name),
			Size: e.rec.Size,
		}
		verr := ValidateEvidence(progress.Within(ctx, ph), db, e.id, e.rec, ValidateContent)
		switch {
		case verr == nil:
			report.Verified++
		case errors.Is(verr, ErrMaterialMissing), errors.Is(verr, ErrMaterialCorrupt):
			r.Problem = verr.Error()
			report.Problems++
		default:
			return report, verr
		}
		report.Results = append(report.Results, r)
	}
	return report, nil
}
