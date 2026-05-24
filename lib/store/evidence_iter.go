package store

import (
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

// forEachEvidenceRecord iterates evidence records via strict E-prefix keys.
// Callback key argument is the raw evidence UUID (prefix removed), which keeps
// relation/chunk linkage code paths stable.
func forEachEvidenceRecord(txn *badger.Txn, fn func(key []byte, evidenceFile structs.EvidenceFile) error) error {
	eviPrefix := []byte(cnst.EviFileNamespace)
	it := txn.NewIterator(badger.DefaultIteratorOptions)
	defer it.Close()
	for it.Seek(eviPrefix); it.ValidForPrefix(eviPrefix); it.Next() {
		item := it.Item()
		key := item.KeyCopy(nil)
		value, err := item.ValueCopy(nil)
		if err != nil {
			return err
		}
		evidenceFile, ok, err := decodeEvidenceRecord(value)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		rawID := dbio.EvidenceRawID(key)
		if err := fn(rawID, evidenceFile); err != nil {
			return err
		}
	}

	return nil
}

func decodeEvidenceRecord(value []byte) (structs.EvidenceFile, bool, error) {
	var evidenceFile structs.EvidenceFile

	decoded, decodeErr := cnst.DECODER.DecodeAll(value, nil)
	if decodeErr == nil {
		value = decoded
	}

	if err := msgpack.Unmarshal(value, &evidenceFile); err != nil {
		return evidenceFile, false, nil
	}

	// UUID-keyed layout has no namespace marker. Filter probable evidence records.
	if evidenceFile.EvidenceType == "" && evidenceFile.IngestState == "" && !evidenceFile.Completed && !evidenceFile.Failed {
		return evidenceFile, false, nil
	}

	return evidenceFile, true, nil
}
