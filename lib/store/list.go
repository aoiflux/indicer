package store

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"indicer/lib/cnst"
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

	// Build indexed files data
	var indexedFiles []map[string]interface{}
	for ihash := range pdata.InternalObjects {
		offset := pdata.InternalObjects[ihash]
		ifileData, err := buildIndexedFileData(ihash, txn)
		if err != nil {
			return nil, err
		}
		ifileData["offset"] = map[string]interface{}{
			"start": offset.Start,
			"end":   offset.End,
		}
		indexedFiles = append(indexedFiles, ifileData)
	}

	partData := map[string]interface{}{
		"hash":         phash,
		"type":         pdata.IndexedType,
		"size":         pdata.Size,
		"files":        getMapKeys(pdata.Names),
		"fileCount":    len(pdata.Names),
		"indexedFiles": indexedFiles,
		"indexedCount": len(indexedFiles),
	}

	return partData, nil
}

func buildIndexedFileData(ihash string, txn *badger.Txn) (map[string]interface{}, error) {
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

	filesDetailed := make([]map[string]interface{}, 0, len(cleanedNames))
	for name := range cleanedNames {
		meta := cleanedMeta[name]
		filesDetailed = append(filesDetailed, map[string]interface{}{
			"name":         name,
			"isDeleted":    meta.IsDeleted,
			"isFragmented": meta.IsFragmented,
		})
	}

	ifileData := map[string]interface{}{
		"hash":          ihash,
		"type":          idata.IndexedType,
		"size":          idata.Size,
		"start":         idata.Start,
		"isDeleted":     idata.IsDeleted,
		"files":         getMapKeys(cleanedNames),
		"fileCount":     len(cleanedNames),
		"filesDetailed": filesDetailed,
	}

	return ifileData, nil
}

func getMapKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
