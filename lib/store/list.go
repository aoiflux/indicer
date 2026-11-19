package store

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"indicer/internal/console"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

func List(db *badger.DB, statusFilter string) error {
	out, err := listOutput(db, statusFilter)
	if err != nil {
		return err
	}
	jsonData, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	console.Println(string(jsonData))
	return nil
}

// listOutput builds the listing of the database's evidence.
func listOutput(db *badger.DB, statusFilter string) (map[string]interface{}, error) {
	statusFilter = normalizeStatusFilter(statusFilter)
	var output map[string]interface{}
	err := db.View(func(txn *badger.Txn) error {
		lookup := newPartitionLookup(txn)
		var evidenceFiles []map[string]interface{}
		var completedCount, pendingCount, failedCount int

		err := forEachEvidenceRecord(txn, func(k []byte, evidata structs.EvidenceFile) error {

			status := evidenceStatus(evidata.Completed, evidata.Failed)
			switch status {
			case "completed":
				completedCount++
			case "failed":
				failedCount++
			default:
				pendingCount++
			}
			if !matchesStatusFilter(statusFilter, status) {
				return nil
			}

			evidenceID := base64.StdEncoding.EncodeToString(k)
			hashStr := evidata.FileHash
			if hashStr == "" {
				hashStr = evidenceID
			}

			// Build partition data. A quarantined index is known wrong
			// (ING-7): its partitions are not listed.
			var partitions []map[string]interface{}
			quarantined := evidata.Index != nil && evidata.Index.State == structs.IndexInvalid
			if evidata.Completed && !quarantined {
				partitionHashes, err := lookup.hashes(evidenceID, evidata)
				if err != nil {
					if errors.Is(err, badger.ErrKeyNotFound) {
						partitionHashes = nil
					} else {
						return err
					}
				}
				for _, phash := range partitionHashes {
					partData, err := buildPartitionData(phash, txn)
					if err != nil {
						if errors.Is(err, badger.ErrKeyNotFound) {
							continue
						}
						return err
					}
					partitions = append(partitions, partData)
				}
			}

			eviFile := map[string]interface{}{
				"id":             evidenceID,
				"hash":           hashStr,
				"name":           normalizeEvidenceFileName(CleanName(evidata.Name)),
				"type":           evidata.EvidenceType,
				"size":           evidata.Size,
				"completed":      evidata.Completed,
				"failed":         evidata.Failed,
				"status":         status,
				"fileCount":      1,
				"partitions":     partitions,
				"partitionCount": len(partitions),
			}
			if m := evidata.Merged; m != nil {
				eviFile["merged"] = mergedListing(*m)
			}
			if evidata.Index != nil {
				eviFile["indexState"] = string(evidata.Index.State)
				if evidata.Index.Reason != "" {
					eviFile["indexReason"] = evidata.Index.Reason
				}
			}
			evidenceFiles = append(evidenceFiles, eviFile)
			return nil
		})
		if err != nil {
			return err
		}

		visibleCompleted := 0
		visiblePending := 0
		visibleFailed := 0
		for _, evidence := range evidenceFiles {
			status, _ := evidence["status"].(string)
			switch status {
			case "completed":
				visibleCompleted++
			case "failed":
				visibleFailed++
			default:
				visiblePending++
			}
		}

		output = map[string]interface{}{
			"statusFilter":      statusFilter,
			"totalEvidence":     len(evidenceFiles),
			"completedEvidence": visibleCompleted,
			"pendingEvidence":   visiblePending,
			"failedEvidence":    visibleFailed,
			"scanCompleted":     completedCount,
			"scanPending":       pendingCount,
			"scanFailed":        failedCount,
			"evidence":          evidenceFiles,
		}
		return nil
	})
	return output, err
}

// mergedListing is where a merged evidence came from, as list shows it.
func mergedListing(m structs.MergeInfo) map[string]any {
	out := map[string]any{
		"package":          m.Package,
		"sourceEvidenceId": base64.StdEncoding.EncodeToString(m.SourceEvidenceID),
		"at":               time.Unix(0, m.At).UTC().Format(time.RFC3339),
	}
	if m.SourceActor != "" {
		out["sourceActor"] = m.SourceActor
		out["trusted"] = m.Trusted
	}
	return out
}
