package store

import (
	"bytes"
	"encoding/base64"
	"errors"
	"sort"
	"strings"
	"sync/atomic"

	"indicer/internal/msgpackx"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

func buildPartitionData(phash string, txn *badger.Txn) (map[string]interface{}, error) {
	pdata, err := getPartitionByHash(txn, phash)
	if err != nil {
		return nil, err
	}

	pid, err := base64.StdEncoding.DecodeString(phash)
	if err != nil {
		return nil, err
	}
	var indexedFiles []map[string]interface{}
	err = dbio.ForEachPartitionMemberTxn(txn, pid, pdata, func(ihash string, offset structs.InternalOffset) error {
		ifileDataEntries, err := buildIndexedFileData(ihash, txn)
		if err != nil {
			if errors.Is(err, badger.ErrKeyNotFound) {
				return nil
			}
			return err
		}
		for _, ifileData := range ifileDataEntries {
			ifileData["offset"] = map[string]interface{}{
				"start": offset.Start,
				"end":   offset.End,
			}
			indexedFiles = append(indexedFiles, ifileData)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	partData := map[string]interface{}{
		"id":           phash,
		"hash":         pdata.FileHash,
		"fileName":     dbio.RecordPath(pdata.IndexedFile),
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

		if unmarshalErr := msgpackx.Unmarshal(v, &pdata); unmarshalErr != nil {
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

		if unmarshalErr := msgpackx.Unmarshal(v, &idata); unmarshalErr != nil {
			continue
		}
		found = true
		break
	}
	if !found {
		return nil, badger.ErrKeyNotFound
	}

	// The path, from the record's own field or its legacy name; splitting
	// the name broke paths holding the separator (SRC-8).
	name := dbio.RecordPath(idata)
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

// partitionScans counts the scans for partitions named after their evidence.
var partitionScans atomic.Int64

// partitionLookup finds the partitions of the evidence a listing shows.
// Evidence records link their partitions (InternalObjects). Records of the
// oldest layout may not, and their partitions are found by name, which needs
// a scan of the whole database: it is made once per lookup, when the first
// such record turns up. Records written since W6 link every partition, so an
// evidence of that kind without links has none.
type partitionLookup struct {
	txn *badger.Txn
	// byName maps an evidence hash to the partitions named after it; nil
	// until the scan.
	byName map[string][]string
}

func newPartitionLookup(txn *badger.Txn) *partitionLookup {
	return &partitionLookup{txn: txn}
}

func (l *partitionLookup) hashes(evidenceHash string, evidata structs.EvidenceFile) ([]string, error) {
	if len(evidata.InternalObjects) > 0 {
		partitionHashes := make([]string, 0, len(evidata.InternalObjects))
		for phash := range evidata.InternalObjects {
			if _, err := getPartitionByHash(l.txn, phash); err != nil {
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
	if evidata.ContentModel == structs.ContentModelV2 {
		return nil, nil
	}
	if l.byName == nil {
		if err := l.scan(); err != nil {
			return nil, err
		}
	}
	return l.byName[evidenceHash], nil
}

// scan collects every partition record by the evidence its name starts with.
func (l *partitionLookup) scan() error {
	partitionScans.Add(1)
	l.byName = map[string][]string{}
	iter := l.txn.NewIterator(badger.DefaultIteratorOptions)
	defer iter.Close()
	for iter.Rewind(); iter.Valid(); iter.Next() {
		item := iter.Item()
		value, err := item.ValueCopy(nil)
		if err != nil {
			return err
		}
		pdata, ok, err := decodePartitionRecord(value)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		owner, _, _ := strings.Cut(pdata.Name, cnst.DataSeperator)
		rawID := bytes.TrimPrefix(item.KeyCopy(nil), []byte(cnst.PartiFileNamespace))
		l.byName[owner] = append(l.byName[owner], base64.StdEncoding.EncodeToString(rawID))
	}
	for _, hashes := range l.byName {
		sort.Strings(hashes)
	}
	return nil
}

func decodePartitionRecord(value []byte) (structs.PartitionFile, bool, error) {
	var pdata structs.PartitionFile

	decoded, decodeErr := cnst.DECODER.DecodeAll(value, nil)
	if decodeErr == nil {
		value = decoded
	}

	if err := msgpackx.Unmarshal(value, &pdata); err != nil {
		return pdata, false, nil
	}

	if pdata.Name == "" || pdata.Size == 0 {
		return pdata, false, nil
	}

	return pdata, true, nil
}
