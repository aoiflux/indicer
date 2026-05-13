package store

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"indicer/lib/cnst"
	"indicer/lib/enrichment"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

func List(db *badger.DB) error {
	return db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 1000
		it := txn.NewIterator(opts)
		defer it.Close()

		eviPrefix := []byte(cnst.EviFileNamespace)
		var evidenceFiles []map[string]interface{}

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

			if !evidata.Completed {
				continue
			}

			evihash := bytes.Split(k, eviPrefix)[1]
			hashStr := base64.StdEncoding.EncodeToString(evihash)

			// Build partition data
			var partitions []map[string]interface{}
			for phash := range evidata.InternalObjects {
				partData, err := buildPartitionData(phash, txn)
				if err != nil {
					return err
				}
				partitions = append(partitions, partData)
			}

			eviFile := map[string]interface{}{
				"hash":           hashStr,
				"name":           normalizeEvidenceFileName(firstCleanNameFromMap(evidata.Names)),
				"type":           evidata.EvidenceType,
				"size":           evidata.Size,
				"completed":      evidata.Completed,
				"files":          getMapKeys(evidata.Names),
				"fileCount":      len(evidata.Names),
				"partitions":     partitions,
				"partitionCount": len(partitions),
			}
			evidenceFiles = append(evidenceFiles, eviFile)
		}

		// Output as JSON
		output := map[string]interface{}{
			"totalEvidence": len(evidenceFiles),
			"evidence":      evidenceFiles,
		}

		jsonData, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return err
		}

		fmt.Println(string(jsonData))
		return nil
	})
}

func buildPartitionData(phash string, txn *badger.Txn) (map[string]interface{}, error) {
	decodedPhash, err := base64.StdEncoding.DecodeString(phash)
	if err != nil {
		return nil, err
	}

	pid := util.AppendToBytesSlice(cnst.PartiFileNamespace, decodedPhash)
	item, err := txn.Get(pid)
	if err != nil {
		return nil, err
	}
	v, err := item.ValueCopy(nil)
	if err != nil {
		return nil, err
	}

	decoded, err := cnst.DECODER.DecodeAll(v, nil)
	if err == nil {
		v = decoded
	}

	var pdata structs.PartitionFile
	err = msgpack.Unmarshal(v, &pdata)
	if err != nil {
		return nil, err
	}

	// Build indexed files data as one output node per fullpath.
	var indexedFiles []map[string]interface{}
	for ihash := range pdata.InternalObjects {
		offset := pdata.InternalObjects[ihash]
		ifileDataEntries, err := buildIndexedFileData(ihash, txn)
		if err != nil {
			return nil, err
		}
		for _, ifileData := range ifileDataEntries {
			ifileData["offset"] = map[string]interface{}{
				"start": offset.Start,
				"end":   offset.End,
			}
			indexedFiles = append(indexedFiles, ifileData)
		}
	}

	partData := map[string]interface{}{
		"hash":         phash,
		"fileName":     firstCleanNameFromMap(pdata.Names),
		"type":         pdata.IndexedType,
		"size":         pdata.Size,
		"files":        getMapKeys(pdata.Names),
		"fileCount":    len(pdata.Names),
		"indexedFiles": indexedFiles,
		"indexedCount": len(indexedFiles),
	}

	return partData, nil
}

func buildIndexedFileData(ihash string, txn *badger.Txn) ([]map[string]interface{}, error) {
	decodedIhash, err := base64.StdEncoding.DecodeString(ihash)
	if err != nil {
		return nil, err
	}

	iid := util.AppendToBytesSlice(cnst.IdxFileNamespace, decodedIhash)
	item, err := txn.Get(iid)
	if err != nil {
		return nil, err
	}
	v, err := item.ValueCopy(nil)
	if err != nil {
		return nil, err
	}

	decoded, err := cnst.DECODER.DecodeAll(v, nil)
	if err == nil {
		v = decoded
	}

	var idata structs.IndexedFile
	err = msgpack.Unmarshal(v, &idata)
	if err != nil {
		return nil, err
	}

	cleanedNames := make(map[string]struct{}, len(idata.Names))
	cleanedMeta := make(map[string]structs.IndexedNameMeta, len(idata.Names))
	for rawName := range idata.Names {
		name := rawName
		if strings.Contains(rawName, cnst.DataSeperator) {
			parts := strings.Split(rawName, cnst.DataSeperator)
			if len(parts) >= 3 {
				name = parts[2]
			}
		}

		cleanedNames[name] = struct{}{}

		meta := structs.IndexedNameMeta{IsDeleted: idata.IsDeleted}
		if rawMeta, ok := idata.NameMeta[rawName]; ok {
			meta = rawMeta
		}
		existing := cleanedMeta[name]
		existing.IsDeleted = existing.IsDeleted || meta.IsDeleted
		cleanedMeta[name] = existing
	}

	names := getMapKeys(cleanedNames)
	sort.Strings(names)
	if len(names) == 0 {
		names = []string{ihash}
	}

	entries := make([]map[string]interface{}, 0, len(names))
	for _, name := range names {
		meta := cleanedMeta[name]
		entry := map[string]interface{}{
			"hash":         ihash,
			"fileName":     name,
			"path":         name,
			"type":         idata.IndexedType,
			"size":         idata.Size,
			"start":        idata.Start,
			"isDeleted":    idata.IsDeleted || meta.IsDeleted,
			"isFragmented": meta.IsFragmented,
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

func getMapKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// ListWithEnrichment attempts to list data from enrichment graphdb first,
// falling back to KVDB if graphdb is empty or unavailable.
func ListWithEnrichment(db *badger.DB, enrichRepo enrichment.Repository) error {
	// Try to read enrichment hierarchy
	hierarchy, err := enrichRepo.ReadHierarchy()
	if err != nil {
		// Fall back to KVDB-only listing
		return List(db)
	}

	// If no evidence files found in graphdb, fall back to KVDB
	if len(hierarchy.EvidenceFiles) == 0 {
		return List(db)
	}

	// Format graphdb hierarchy into output
	var evidenceFiles []map[string]interface{}

	for _, evidenceFile := range hierarchy.EvidenceFiles {
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

		eviFile := map[string]interface{}{
			"hash":                   evidenceFile.ID,
			"name":                   normalizeEvidenceFileName(evidenceFile.Name),
			"type":                   "image",
			"completed":              true,
			"partitions":             partitions,
			"partitionCount":         len(partitions),
			"indexedCount":           totalIndexed,
			"deletedIndexedCount":    totalDeletedIndexed,
			"fragmentedIndexedCount": totalFragmentedIndexed,
			"source":                 "graphdb",
		}
		evidenceFiles = append(evidenceFiles, eviFile)
	}

	// Output as JSON
	output := map[string]interface{}{
		"totalEvidence": len(evidenceFiles),
		"evidence":      evidenceFiles,
	}

	jsonData, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return err
	}

	fmt.Println(string(jsonData))
	return nil
}

func getMapKeysFromSlice(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func trimHierName(value string) string {
	if strings.Contains(value, cnst.DataSeperator) {
		parts := strings.SplitN(value, cnst.DataSeperator, 3)
		if len(parts) >= 2 {
			return parts[len(parts)-1]
		}
	}
	return value
}

func firstMapKey(values map[string]struct{}) string {
	keys := getMapKeys(values)
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	return keys[0]
}

func firstCleanNameFromMap(values map[string]struct{}) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for name := range values {
		keys = append(keys, trimHierName(name))
	}
	sort.Strings(keys)
	return keys[0]
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

var partitionSuffixPattern = regexp.MustCompile(`_p[0-9]+$`)

func normalizeEvidenceFileName(value string) string {
	name := strings.TrimSpace(trimHierName(value))
	return partitionSuffixPattern.ReplaceAllString(name, "")
}
