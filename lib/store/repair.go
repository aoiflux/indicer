package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

type RepairReport struct {
	TotalProblematic   int                 `json:"totalProblematic"`
	PendingCount       int                 `json:"pendingCount"`
	FlushedCount       int                 `json:"flushedCount"`
	FailedCount        int                 `json:"failedCount"`
	RepairedCount      int                 `json:"repairedCount"`
	AutoCompletedCount int                 `json:"autoCompletedCount"`
	Fixed              bool                `json:"fixed"`
	Evidence           []RepairEvidenceRow `json:"evidence"`
}

type RepairEvidenceRow struct {
	Hash        string `json:"hash"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Size        int64  `json:"size"`
	Completed   bool   `json:"completed"`
	Failed      bool   `json:"failed"`
	IngestState string `json:"ingestState"`
	Action      string `json:"action"`

	id []byte
}

func Repair(db *badger.DB, fix bool) error {
	report, err := InspectEvidenceRepairs(db, fix)
	if err != nil {
		return err
	}

	jsonData, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}

	fmt.Println(string(jsonData))
	return nil
}

func InspectEvidenceRepairs(db *badger.DB, fix bool) (RepairReport, error) {
	var report RepairReport
	report.Fixed = fix

	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 1000
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			key := item.KeyCopy(nil)
			rawID := dbio.EvidenceRawID(key)
			value, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}

			decoded, err := cnst.DECODER.DecodeAll(value, nil)
			if err == nil {
				value = decoded
			}

			var evidenceFile structs.EvidenceFile
			if err := msgpack.Unmarshal(value, &evidenceFile); err != nil {
				continue
			}
			if evidenceFile.EvidenceType == "" && evidenceFile.IngestState == "" && !evidenceFile.Completed && !evidenceFile.Failed {
				continue
			}
			if evidenceFile.Completed {
				continue
			}

			row := RepairEvidenceRow{
				Hash:        base64.StdEncoding.EncodeToString(rawID),
				Name:        firstCleanName(evidenceFile.Name),
				Type:        evidenceFile.EvidenceType,
				Size:        evidenceFile.Size,
				Completed:   evidenceFile.Completed,
				Failed:      evidenceFile.Failed,
				IngestState: string(evidenceFile.IngestState),
				Action:      "inspect",
				id:          rawID,
			}
			if evidenceFile.Failed {
				report.FailedCount++
				row.Action = "already_failed"
			} else if evidenceFile.IngestState == structs.IngestStateFlushed {
				report.FlushedCount++
				row.Action = "flushed"
			} else {
				report.PendingCount++
				row.Action = "pending"
			}
			report.Evidence = append(report.Evidence, row)
		}
		return nil
	})
	if err != nil {
		return report, err
	}

	if fix {
		for index := range report.Evidence {
			if report.Evidence[index].Failed {
				continue
			}
			if report.Evidence[index].IngestState == string(structs.IngestStateFlushed) {
				evidenceFile, err := dbio.GetEvidenceFile(report.Evidence[index].id, db)
				if err != nil {
					return report, err
				}
				if err := ValidateFlushedEvidenceMaterialized(report.Evidence[index].id, evidenceFile, db); err != nil {
					if err := MarkEvidenceFileFailed(report.Evidence[index].id, db); err != nil {
						return report, err
					}
					report.Evidence[index].Failed = true
					report.Evidence[index].Action = "flushed_invalid_marked_failed"
					report.RepairedCount++
					report.FlushedCount--
					report.FailedCount++
					continue
				}

				// FLUSHED with readable chunk material: auto-complete marker write.
				if err := CompleteEvidenceFile(report.Evidence[index].id, db); err != nil {
					return report, err
				}
				report.Evidence[index].Completed = true
				report.Evidence[index].IngestState = string(structs.IngestStateCompleted)
				report.Evidence[index].Action = "auto_completed"
				report.AutoCompletedCount++
				report.FlushedCount--
			} else {
				if err := MarkEvidenceFileFailed(report.Evidence[index].id, db); err != nil {
					return report, err
				}
				report.Evidence[index].Failed = true
				report.Evidence[index].Action = "marked_failed"
				report.RepairedCount++
				report.PendingCount--
				report.FailedCount++
			}
		}
	}

	report.TotalProblematic = len(report.Evidence)
	return report, nil
}
