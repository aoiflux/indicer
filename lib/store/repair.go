package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"indicer/internal/console"
	"strings"

	"indicer/lib/dbio"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

type RepairReport struct {
	TotalProblematic   int `json:"totalProblematic"`
	PendingCount       int `json:"pendingCount"`
	FlushedCount       int `json:"flushedCount"`
	FailedCount        int `json:"failedCount"`
	RepairedCount      int `json:"repairedCount"`
	AutoCompletedCount int `json:"autoCompletedCount"`
	// DuplicateCount counts duplicate attempts whose records are not all
	// removed; AliasedCount, FLUSHED attempts --fix found to be aliases.
	DuplicateCount int `json:"duplicateCount"`
	AliasedCount   int `json:"aliasedCount"`
	// IndexRunningCount counts evidence whose index pass is still marked
	// running; --fix marks it interrupted (IndexInterruptedCount).
	IndexRunningCount     int                 `json:"indexRunningCount"`
	IndexInterruptedCount int                 `json:"indexInterruptedCount"`
	Fixed                 bool                `json:"fixed"`
	Evidence              []RepairEvidenceRow `json:"evidence"`
}

type RepairEvidenceRow struct {
	Hash        string `json:"hash"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Size        int64  `json:"size"`
	Completed   bool   `json:"completed"`
	Failed      bool   `json:"failed"`
	IngestState string `json:"ingestState"`
	IndexState  string `json:"indexState,omitempty"`
	Action      string `json:"action"`

	id []byte
}

func Repair(ctx context.Context, db *badger.DB, fix bool) error {
	report, err := InspectEvidenceRepairs(ctx, db, fix)
	if err != nil {
		return err
	}

	jsonData, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}

	console.Println(string(jsonData))
	return nil
}

// InspectEvidenceRepairs lists the evidence an unfinished ingest left, and
// with fix recovers each one as a writable open does (RecoverEvidence).
func InspectEvidenceRepairs(ctx context.Context, db *badger.DB, fix bool) (RepairReport, error) {
	var report RepairReport
	report.Fixed = fix

	// Only evidence records are inspected; decoding every key in the
	// database as one reported whatever happened to decode (ING-10).
	err := db.View(func(txn *badger.Txn) error {
		return forEachEvidenceRecord(txn, func(rawID []byte, e structs.EvidenceFile) error {
			if e.Completed && !NeedsRecovery(e) {
				return nil
			}
			row := RepairEvidenceRow{
				Hash:        base64.StdEncoding.EncodeToString(rawID),
				Name:        firstCleanName(e.Name),
				Type:        e.EvidenceType,
				Size:        e.Size,
				Completed:   e.Completed,
				Failed:      e.Failed,
				IngestState: string(e.IngestState),
				id:          rawID,
			}
			if e.Index != nil {
				row.IndexState = string(e.Index.State)
			}
			switch {
			case e.IngestState == structs.IngestStateDuplicate:
				report.DuplicateCount++
				row.Action = "duplicate"
			case e.Completed:
				report.IndexRunningCount++
				row.Action = "index_running"
			case e.Failed:
				report.FailedCount++
				row.Action = "already_failed"
				if e.Index != nil && e.Index.State == structs.IndexRunning {
					report.IndexRunningCount++
				}
			case e.IngestState == structs.IngestStateFlushed:
				report.FlushedCount++
				row.Action = "flushed"
			default:
				report.PendingCount++
				row.Action = "pending"
			}
			report.Evidence = append(report.Evidence, row)
			return nil
		})
	})
	if err != nil {
		return report, err
	}

	if fix {
		for i := range report.Evidence {
			if err := ctx.Err(); err != nil {
				return report, context.Cause(ctx)
			}
			row := &report.Evidence[i]
			e, err := dbio.GetEvidenceFile(row.id, db)
			if errors.Is(err, badger.ErrKeyNotFound) {
				continue
			}
			if err != nil {
				return report, err
			}
			if !NeedsRecovery(e) {
				continue
			}
			before := row.Action
			actions, err := RecoverEvidence(ctx, db, row.id, e)
			for _, a := range actions {
				switch a {
				case ActionMarkedFailed, ActionFlushedInvalid:
					report.RepairedCount++
					report.FailedCount++
					row.Failed = true
				case ActionCompleted:
					report.AutoCompletedCount++
					row.Completed, row.IngestState = true, string(structs.IngestStateCompleted)
				case ActionAliased:
					report.AliasedCount++
					row.IngestState = string(structs.IngestStateDuplicate)
				case ActionDuplicateCleaned:
					report.DuplicateCount--
				case ActionIndexInterrupted:
					report.IndexRunningCount--
					report.IndexInterruptedCount++
					row.IndexState = string(structs.IndexInterrupted)
				}
			}
			if len(actions) > 0 {
				switch before {
				case "pending":
					report.PendingCount--
				case "flushed":
					report.FlushedCount--
				}
				row.Action = strings.Join(actions, ",")
			}
			if err != nil {
				return report, err
			}
		}
	}

	report.TotalProblematic = len(report.Evidence)
	return report, nil
}
