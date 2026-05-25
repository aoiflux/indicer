package store

import (
	"bytes"
	"encoding/base64"
	"errors"
	"sort"
	"strings"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
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
			if errors.Is(err, badger.ErrKeyNotFound) {
				continue
			}
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
		"id":           phash,
		"hash":         pdata.FileHash,
		"fileName":     firstCleanName(pdata.Name),
		"type":         pdata.IndexedType,
		"size":         pdata.Size,
		"fileCount":    1,
		"indexedFiles": indexedFiles,
		"indexedCount": len(indexedFiles),
	}
	if partData["hash"] == "" {
		partData["hash"] = phash
	}

	return partData, nil
}

func firstCleanName(name string) string {
	if name == "" {
		return ""
	}
	if strings.Contains(name, cnst.DataSeperator) {
		parts := strings.Split(name, cnst.DataSeperator)
		return parts[len(parts)-1]
	}
	return name
}

func getPartitionByHash(txn *badger.Txn, partitionHash string) (structs.PartitionFile, error) {
	var pdata structs.PartitionFile
	decodedPhash, err := base64.StdEncoding.DecodeString(partitionHash)
	if err != nil {
		return pdata, err
	}

	// Partition IDs are currently UUID primary keys (raw bytes). Support both
	// raw UUID and namespaced key shapes for forward compatibility.
	candidates := [][]byte{
		decodedPhash,
		util.AppendToBytesSlice(cnst.PartiFileNamespace, decodedPhash),
	}

	for _, pid := range candidates {
		item, getErr := txn.Get(pid)
		if getErr == badger.ErrKeyNotFound {
			continue
		}
		if getErr != nil {
			return pdata, getErr
		}
		v, copyErr := item.ValueCopy(nil)
		if copyErr != nil {
			return pdata, copyErr
		}

		decoded, decodeErr := cnst.DECODER.DecodeAll(v, nil)
		if decodeErr == nil {
			v = decoded
		}

		if unmarshalErr := msgpack.Unmarshal(v, &pdata); unmarshalErr != nil {
			continue
		}
		return pdata, nil
	}

	return pdata, badger.ErrKeyNotFound
}

func buildIndexedFileData(ihash string, txn *badger.Txn) ([]map[string]interface{}, error) {
	decodedIhash, err := base64.StdEncoding.DecodeString(ihash)
	if err != nil {
		return nil, err
	}

	var idata structs.IndexedFile
	// Indexed IDs are currently UUID primary keys (raw bytes). Support both
	// raw UUID and namespaced key shapes for forward compatibility.
	candidates := [][]byte{
		decodedIhash,
		util.AppendToBytesSlice(cnst.IdxFileNamespace, decodedIhash),
	}

	found := false
	for _, iid := range candidates {
		item, getErr := txn.Get(iid)
		if getErr == badger.ErrKeyNotFound {
			continue
		}
		if getErr != nil {
			return nil, getErr
		}
		v, copyErr := item.ValueCopy(nil)
		if copyErr != nil {
			return nil, copyErr
		}

		decoded, decodeErr := cnst.DECODER.DecodeAll(v, nil)
		if decodeErr == nil {
			v = decoded
		}

		if unmarshalErr := msgpack.Unmarshal(v, &idata); unmarshalErr != nil {
			continue
		}
		found = true
		break
	}
	if !found {
		return nil, badger.ErrKeyNotFound
	}

	name := firstCleanName(idata.Name)
	if name == "" {
		name = ihash
	}
	entries := []map[string]interface{}{{
		"id":           ihash,
		"hash":         idata.FileHash,
		"fileName":     name,
		"path":         name,
		"type":         idata.IndexedType,
		"size":         idata.Size,
		"start":        idata.Start,
		"isDeleted":    idata.IsDeleted,
		"isFragmented": idata.IsFragmented,
	}}
	if entries[0]["hash"] == "" {
		entries[0]["hash"] = ihash
	}

	return entries, nil
}

func buildCompletedEvidenceFromKV(evidenceHash string, db *badger.DB) (map[string]interface{}, bool, error) {
	result := map[string]interface{}{}
	err := db.View(func(txn *badger.Txn) error {
		evidata, evidenceID, found, err := getEvidenceByHash(txn, evidenceHash)
		if err != nil {
			return err
		}
		if !found || !evidata.Completed {
			return nil
		}
		if evidenceID == "" {
			evidenceID = evidenceHash
		}

		partitionHashes, err := getEvidencePartitionHashes(txn, evidenceID, evidata)
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

		result["name"] = normalizeEvidenceFileName(firstCleanName(evidata.Name))
		result["id"] = evidenceID
		result["hash"] = evidata.FileHash
		result["type"] = evidata.EvidenceType
		result["size"] = evidata.Size
		result["fileCount"] = 1
		result["partitions"] = partitions
		result["partitionCount"] = len(partitions)
		result["indexedCount"] = totalIndexed
		result["deletedIndexedCount"] = totalDeletedIndexed
		result["fragmentedIndexedCount"] = totalFragmentedIndexed
		if result["hash"] == "" {
			result["hash"] = evidenceID
		}

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
			if _, err := getPartitionByHash(txn, phash); err != nil {
				if errors.Is(err, badger.ErrKeyNotFound) {
					continue
				}
				return nil, err
			}
			partitionHashes = append(partitionHashes, phash)
		}
		sort.Strings(partitionHashes)
		return partitionHashes, nil
	}

	partitionHashes := make([]string, 0)
	iter := txn.NewIterator(badger.DefaultIteratorOptions)
	defer iter.Close()

	for iter.Rewind(); iter.Valid(); iter.Next() {
		item := iter.Item()
		key := item.KeyCopy(nil)
		value, err := item.ValueCopy(nil)
		if err != nil {
			return nil, err
		}

		pdata, ok, err := decodePartitionRecord(value)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if !partitionBelongsToEvidence(evidenceHash, pdata) {
			continue
		}

		rawID := key
		if bytes.HasPrefix(rawID, []byte(cnst.PartiFileNamespace)) {
			rawID = bytes.TrimPrefix(rawID, []byte(cnst.PartiFileNamespace))
		}
		partitionHashes = append(partitionHashes, base64.StdEncoding.EncodeToString(rawID))
	}

	sort.Strings(partitionHashes)
	return partitionHashes, nil
}

func decodePartitionRecord(value []byte) (structs.PartitionFile, bool, error) {
	var pdata structs.PartitionFile

	decoded, decodeErr := cnst.DECODER.DecodeAll(value, nil)
	if decodeErr == nil {
		value = decoded
	}

	if err := msgpack.Unmarshal(value, &pdata); err != nil {
		return pdata, false, nil
	}

	if pdata.Name == "" || pdata.Size == 0 {
		return pdata, false, nil
	}

	return pdata, true, nil
}

func partitionBelongsToEvidence(evidenceHash string, pdata structs.PartitionFile) bool {
	parts := strings.SplitN(pdata.Name, cnst.DataSeperator, 2)
	return len(parts) > 0 && parts[0] == evidenceHash
}

func getEvidenceByHash(txn *badger.Txn, evidenceHash string) (structs.EvidenceFile, string, bool, error) {
	var evidata structs.EvidenceFile
	rawHash, decodeErr := base64.StdEncoding.DecodeString(evidenceHash)
	if decodeErr != nil {
		rawHash = []byte(evidenceHash)
	}

	candidateKeys := [][]byte{dbio.CanonicalEvidenceKey(rawHash)}

	for _, candidate := range candidateKeys {
		item, err := txn.Get(candidate)
		if err == badger.ErrKeyNotFound {
			continue
		}
		if err != nil {
			return evidata, "", false, err
		}

		v, err := item.ValueCopy(nil)
		if err != nil {
			return evidata, "", false, err
		}

		decoded, err := cnst.DECODER.DecodeAll(v, nil)
		if err == nil {
			v = decoded
		}

		if err := msgpack.Unmarshal(v, &evidata); err != nil {
			continue
		}
		evidenceID := base64.StdEncoding.EncodeToString(dbio.EvidenceRawID(candidate))
		return evidata, evidenceID, true, nil
	}

	// evidenceHash may be a content hash; resolve to UUID key via EH reverse index.
	lookupKey := util.AppendToBytesSlice(cnst.EviFileHashLookupNamespace, evidenceHash)
	item, err := txn.Get(lookupKey)
	if err == badger.ErrKeyNotFound {
		return evidata, "", false, nil
	}
	if err != nil {
		return evidata, "", false, err
	}
	var resolvedIDs [][]byte
	err = item.Value(func(val []byte) error {
		return msgpack.Unmarshal(val, &resolvedIDs)
	})
	if err != nil {
		return evidata, "", false, err
	}
	if len(resolvedIDs) == 0 {
		return evidata, "", false, nil
	}
	resolvedID := resolvedIDs[len(resolvedIDs)-1]

	item, err = txn.Get(resolvedID)
	if err == badger.ErrKeyNotFound {
		return evidata, "", false, nil
	}
	if err != nil {
		return evidata, "", false, err
	}
	v, err := item.ValueCopy(nil)
	if err != nil {
		return evidata, "", false, err
	}
	decoded, err := cnst.DECODER.DecodeAll(v, nil)
	if err == nil {
		v = decoded
	}
	if err := msgpack.Unmarshal(v, &evidata); err != nil {
		return evidata, "", false, err
	}

	evidenceID := base64.StdEncoding.EncodeToString(dbio.EvidenceRawID(resolvedID))
	return evidata, evidenceID, true, nil
}
