package store

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

func List(db *badger.DB, statusFilter string) error {
	statusFilter = normalizeStatusFilter(statusFilter)
	return db.View(func(txn *badger.Txn) error {
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

			// Build partition data
			var partitions []map[string]interface{}
			if evidata.Completed {
				partitionHashes, err := getEvidencePartitionHashes(txn, evidenceID, evidata)
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
				"name":           normalizeEvidenceFileName(firstCleanName(evidata.Name)),
				"type":           evidata.EvidenceType,
				"size":           evidata.Size,
				"completed":      evidata.Completed,
				"failed":         evidata.Failed,
				"status":         status,
				"fileCount":      1,
				"partitions":     partitions,
				"partitionCount": len(partitions),
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

		// Output as JSON
		output := map[string]interface{}{
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

		jsonData, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return err
		}

		fmt.Println(string(jsonData))
		return nil
	})
}
