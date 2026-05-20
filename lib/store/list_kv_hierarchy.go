package store

import (
	"bytes"
	"encoding/base64"
	"sort"
	"strings"

	"indicer/lib/cnst"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

func buildPartitionData(phash string, txn *badger.Txn) (map[string]interface{}, error) {
	pdata, err := getPartitionByHash(txn, phash)
	if err != nil {
		return nil, err
	}

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

func getPartitionByHash(txn *badger.Txn, partitionHash string) (structs.PartitionFile, error) {
	var pdata structs.PartitionFile
	decodedPhash, err := base64.StdEncoding.DecodeString(partitionHash)
	if err != nil {
		return pdata, err
	}

	pid := util.AppendToBytesSlice(cnst.PartiFileNamespace, decodedPhash)
	item, err := txn.Get(pid)
	if err != nil {
		return pdata, err
	}
	v, err := item.ValueCopy(nil)
	if err != nil {
		return pdata, err
	}

	decoded, err := cnst.DECODER.DecodeAll(v, nil)
	if err == nil {
		v = decoded
	}

	err = msgpack.Unmarshal(v, &pdata)
	return pdata, err
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

func buildCompletedEvidenceFromKV(evidenceHash string, db *badger.DB) (map[string]interface{}, bool, error) {
	result := map[string]interface{}{}
	err := db.View(func(txn *badger.Txn) error {
		evidata, found, err := getEvidenceByHash(txn, evidenceHash)
		if err != nil {
			return err
		}
		if !found || !evidata.Completed {
			return nil
		}

		partitionHashes, err := getEvidencePartitionHashes(txn, evidenceHash, evidata)
		if err != nil {
			return err
		}

		partitions := make([]map[string]interface{}, 0, len(partitionHashes))
		totalIndexed := 0
		totalDeletedIndexed := 0
		totalFragmentedIndexed := 0

		for _, phash := range partitionHashes {
			partData, err := buildPartitionData(phash, txn)
			if err != nil {
				return err
			}
			partitions = append(partitions, partData)
			if count, ok := partData["indexedCount"].(int); ok {
				totalIndexed += count
			}
			if indexedFiles, ok := partData["indexedFiles"].([]map[string]interface{}); ok {
				for _, indexed := range indexedFiles {
					if deleted, ok := indexed["isDeleted"].(bool); ok && deleted {
						totalDeletedIndexed++
					}
					if fragmented, ok := indexed["isFragmented"].(bool); ok && fragmented {
						totalFragmentedIndexed++
					}
				}
			}
		}

		result["name"] = normalizeEvidenceFileName(firstCleanNameFromMap(evidata.Names))
		result["type"] = evidata.EvidenceType
		result["size"] = evidata.Size
		result["files"] = getMapKeys(evidata.Names)
		result["fileCount"] = len(evidata.Names)
		result["partitions"] = partitions
		result["partitionCount"] = len(partitions)
		result["indexedCount"] = totalIndexed
		result["deletedIndexedCount"] = totalDeletedIndexed
		result["fragmentedIndexedCount"] = totalFragmentedIndexed

		return nil
	})
	if err != nil {
		return nil, false, err
	}

	if len(result) == 0 {
		return nil, false, nil
	}
	return result, true, nil
}

func getEvidencePartitionHashes(txn *badger.Txn, evidenceHash string, evidata structs.EvidenceFile) ([]string, error) {
	if len(evidata.InternalObjects) > 0 {
		partitionHashes := make([]string, 0, len(evidata.InternalObjects))
		for phash := range evidata.InternalObjects {
			partitionHashes = append(partitionHashes, phash)
		}
		sort.Strings(partitionHashes)
		return partitionHashes, nil
	}

	partitionHashes := make([]string, 0)
	partitionPrefix := []byte(cnst.PartiFileNamespace)
	iter := txn.NewIterator(badger.DefaultIteratorOptions)
	defer iter.Close()

	for iter.Seek(partitionPrefix); iter.ValidForPrefix(partitionPrefix); iter.Next() {
		key := iter.Item().KeyCopy(nil)
		partitionHash := base64.StdEncoding.EncodeToString(bytes.TrimPrefix(key, partitionPrefix))
		pdata, err := getPartitionByHash(txn, partitionHash)
		if err != nil {
			return nil, err
		}
		if partitionBelongsToEvidence(evidenceHash, pdata) {
			partitionHashes = append(partitionHashes, partitionHash)
		}
	}

	sort.Strings(partitionHashes)
	return partitionHashes, nil
}

func partitionBelongsToEvidence(evidenceHash string, pdata structs.PartitionFile) bool {
	for name := range pdata.Names {
		parts := strings.SplitN(name, cnst.DataSeperator, 2)
		if len(parts) > 0 && parts[0] == evidenceHash {
			return true
		}
	}
	return false
}

func getEvidenceByHash(txn *badger.Txn, evidenceHash string) (structs.EvidenceFile, bool, error) {
	var evidata structs.EvidenceFile
	rawHash, err := base64.StdEncoding.DecodeString(evidenceHash)
	if err != nil {
		rawHash = []byte(evidenceHash)
	}

	eid := util.AppendToBytesSlice(cnst.EviFileNamespace, rawHash)
	item, err := txn.Get(eid)
	if err == badger.ErrKeyNotFound {
		return evidata, false, nil
	}
	if err != nil {
		return evidata, false, err
	}

	v, err := item.ValueCopy(nil)
	if err != nil {
		return evidata, false, err
	}

	decoded, err := cnst.DECODER.DecodeAll(v, nil)
	if err == nil {
		v = decoded
	}

	if err := msgpack.Unmarshal(v, &evidata); err != nil {
		return evidata, false, err
	}

	return evidata, true, nil
}
