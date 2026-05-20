package store

import (
	"encoding/json"
	"fmt"
	"strings"

	"indicer/lib/enrichment"

	"github.com/dgraph-io/badger/v4"
)

// ListWithEnrichment reads graph hierarchy first, then backfills missing
// partition/indexed details from KV so list output remains complete.
func ListWithEnrichment(db *badger.DB, enrichRepo enrichment.Repository, statusFilter string) error {
	statusFilter = normalizeStatusFilter(statusFilter)

	hierarchy, err := enrichRepo.ReadHierarchy()
	if err != nil {
		return List(db, statusFilter)
	}
	if len(hierarchy.EvidenceFiles) == 0 {
		return List(db, statusFilter)
	}

	var evidenceFiles []map[string]interface{}
	for _, evidenceFile := range hierarchy.EvidenceFiles {
		if !matchesStatusFilter(statusFilter, "completed") {
			continue
		}

		kvEvidence, hasKVEvidence, err := buildCompletedEvidenceFromKV(evidenceFile.ID, db)
		if err != nil {
			return err
		}

		var partitions []map[string]interface{}
		totalIndexed := 0
		totalDeletedIndexed := 0
		totalFragmentedIndexed := 0

		for _, partition := range evidenceFile.Partitions {
			var indexedFiles []map[string]interface{}
			partitionFileName := trimHierName(partition.Name)
			if partitionFileName == "" {
				partitionFileName = trimHierName(partition.ID)
			}
			deletedIndexed := 0
			fragmentedIndexed := 0

			for _, file := range partition.Files {
				entry := buildEnrichedIndexedFileData(file)
				if deleted, ok := entry["isDeleted"].(bool); ok && deleted {
					deletedIndexed++
				}
				if fragmented, ok := entry["isFragmented"].(bool); ok && fragmented {
					fragmentedIndexed++
				}
				indexedFiles = append(indexedFiles, entry)
			}

			totalIndexed += len(indexedFiles)
			totalDeletedIndexed += deletedIndexed
			totalFragmentedIndexed += fragmentedIndexed

			partitions = append(partitions, map[string]interface{}{
				"hash":                   partition.ID,
				"fileName":               partitionFileName,
				"name":                   partition.Name,
				"type":                   "partition",
				"indexedFiles":           indexedFiles,
				"indexedCount":           len(indexedFiles),
				"deletedIndexedCount":    deletedIndexed,
				"fragmentedIndexedCount": fragmentedIndexed,
			})
		}

		if hasKVEvidence {
			kvPartitions, _ := kvEvidence["partitions"].([]map[string]interface{})
			kvIndexedCount, _ := kvEvidence["indexedCount"].(int)
			if len(partitions) == 0 || (totalIndexed == 0 && kvIndexedCount > 0) {
				partitions = kvPartitions
				totalIndexed, _ = kvEvidence["indexedCount"].(int)
				totalDeletedIndexed, _ = kvEvidence["deletedIndexedCount"].(int)
				totalFragmentedIndexed, _ = kvEvidence["fragmentedIndexedCount"].(int)
			}
		}

		evidenceName := normalizeEvidenceFileName(evidenceFile.Name)
		evidenceType := "image"
		var evidenceSize int64
		files := []string{}
		fileCount := 0
		if hasKVEvidence {
			if val, ok := kvEvidence["name"].(string); ok && strings.TrimSpace(val) != "" {
				evidenceName = normalizeEvidenceFileName(val)
			}
			if val, ok := kvEvidence["type"].(string); ok && strings.TrimSpace(val) != "" {
				evidenceType = val
			}
			if val, ok := kvEvidence["size"].(int64); ok {
				evidenceSize = val
			}
			if val, ok := kvEvidence["files"].([]string); ok {
				files = val
			}
			if val, ok := kvEvidence["fileCount"].(int); ok {
				fileCount = val
			}
		}

		evidenceFiles = append(evidenceFiles, map[string]interface{}{
			"hash":                   evidenceFile.ID,
			"name":                   evidenceName,
			"type":                   evidenceType,
			"size":                   evidenceSize,
			"completed":              true,
			"failed":                 false,
			"status":                 "completed",
			"files":                  files,
			"fileCount":              fileCount,
			"partitions":             partitions,
			"partitionCount":         len(partitions),
			"indexedCount":           totalIndexed,
			"deletedIndexedCount":    totalDeletedIndexed,
			"fragmentedIndexedCount": totalFragmentedIndexed,
			"source":                 "graphdb",
		})
	}

	repairReport, err := InspectEvidenceRepairs(db, false)
	if err != nil {
		return err
	}

	filteredIncomplete := make([]RepairEvidenceRow, 0, len(repairReport.Evidence))
	for _, row := range repairReport.Evidence {
		status := evidenceStatus(row.Completed, row.Failed)
		if matchesStatusFilter(statusFilter, status) {
			filteredIncomplete = append(filteredIncomplete, row)
		}
	}

	completedVisible := len(evidenceFiles)
	pendingVisible := 0
	failedVisible := 0
	for _, row := range filteredIncomplete {
		if row.Failed {
			failedVisible++
		} else {
			pendingVisible++
		}
	}
	if statusFilter == "pending" || statusFilter == "failed" {
		completedVisible = 0
	}

	output := map[string]interface{}{
		"statusFilter":       statusFilter,
		"totalEvidence":      completedVisible + len(filteredIncomplete),
		"completedEvidence":  completedVisible,
		"pendingEvidence":    pendingVisible,
		"failedEvidence":     failedVisible,
		"incompleteEvidence": filteredIncomplete,
		"evidence":           evidenceFiles,
	}

	jsonData, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return err
	}

	fmt.Println(string(jsonData))
	return nil
}

func buildEnrichedIndexedFileData(file *enrichment.EnrichmentFileNode) map[string]interface{} {
	name := trimHierName(file.FileName)
	if name == "" {
		name = trimHierName(file.Path)
	}
	if name == "" {
		name = file.Hash
	}
	path := file.Path
	if path == "" {
		path = name
	}

	return map[string]interface{}{
		"id":           file.ID,
		"hash":         file.Hash,
		"fileName":     name,
		"path":         path,
		"type":         file.FileType,
		"size":         file.Size,
		"entropy":      file.Entropy,
		"hasEntropy":   file.HasEntropy,
		"isDeleted":    file.IsDeleted,
		"isFragmented": file.IsFragmented,
		"mimeType":     file.MimeType,
		"tags":         file.Tags,
	}
}
