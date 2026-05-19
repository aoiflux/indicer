package store

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"indicer/lib/cnst"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

func List(db *badger.DB, statusFilter string) error {
	statusFilter = normalizeStatusFilter(statusFilter)
	return db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 1000
		it := txn.NewIterator(opts)
		defer it.Close()

		eviPrefix := []byte(cnst.EviFileNamespace)
		var evidenceFiles []map[string]interface{}
		var completedCount, pendingCount, failedCount int

		for it.Seek(eviPrefix); it.ValidForPrefix(eviPrefix); it.Next() {
			item := it.Item()
			k := item.KeyCopy(nil)
			v, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}

			decoded, err := cnst.DECODER.DecodeAll(v, nil)
			if err == nil {
				v = decoded
			}

			var evidata structs.EvidenceFile
			err = msgpack.Unmarshal(v, &evidata)
			if err != nil {
				return err
			}

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
				continue
			}

			evihash := bytes.Split(k, eviPrefix)[1]
			hashStr := base64.StdEncoding.EncodeToString(evihash)

			// Build partition data
			var partitions []map[string]interface{}
			if evidata.Completed {
				partitionHashes, err := getEvidencePartitionHashes(txn, hashStr, evidata)
				if err != nil {
					return err
				}
				for _, phash := range partitionHashes {
					partData, err := buildPartitionData(phash, txn)
					if err != nil {
						return err
					}
					partitions = append(partitions, partData)
				}
			}

			eviFile := map[string]interface{}{
				"hash":           hashStr,
				"name":           normalizeEvidenceFileName(firstCleanNameFromMap(evidata.Names)),
				"type":           evidata.EvidenceType,
				"size":           evidata.Size,
				"completed":      evidata.Completed,
				"failed":         evidata.Failed,
				"status":         status,
				"files":          getMapKeys(evidata.Names),
				"fileCount":      len(evidata.Names),
				"partitions":     partitions,
				"partitionCount": len(partitions),
			}
			evidenceFiles = append(evidenceFiles, eviFile)
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
