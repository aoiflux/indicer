package store

import (
	"bytes"
	"indicer/lib/constant"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v3"
	"golang.org/x/exp/slices"
)

func Store(infile structs.InputFile) error {
	if bytes.HasPrefix(infile.GetID(), []byte(constant.IndexedFileNamespace)) {
		return storeIndexedFile(infile)
	}

	if bytes.HasPrefix(infile.GetID(), []byte(constant.PartitionFileNamespace)) {
		return storePartitionFile(infile)
	}

	return storeEvidenceFile(infile)
}

func storeIndexedFile(infile structs.InputFile) error {
	indexedFile, err := getIndexedFile(infile.GetID(), infile.GetDB())
	if err != nil && err == badger.ErrKeyNotFound {
		indexedFile = structs.NewIndexedFile(
			infile.GetName(),
			infile.GetStartIndex(),
			infile.GetSize(),
		)
		return setFile(infile.GetID(), indexedFile, infile.GetDB())
	}
	if err != nil && err != badger.ErrKeyNotFound {
		return err
	}

	if slices.Contains(indexedFile.Names, infile.GetName()) {
		return nil
	}

	indexedFile.Names = append(indexedFile.Names, infile.GetName())
	return setFile(infile.GetID(), indexedFile, infile.GetDB())
}
func storePartitionFile(infile structs.InputFile) error {
	partitionFile, err := getPartitionFile(infile.GetID(), infile.GetDB())
	if err != nil && err == badger.ErrKeyNotFound {
		partitionFile = structs.NewPartitionFile(
			infile.GetName(),
			infile.GetStartIndex(),
			infile.GetSize(),
			infile.GetInternalObjects(),
		)
		return setFile(infile.GetID(), partitionFile, infile.GetDB())
	}
	if err != nil && err != badger.ErrKeyNotFound {
		return err
	}

	if slices.Contains(partitionFile.Names, infile.GetName()) {
		return nil
	}

	partitionFile.Names = append(partitionFile.Names, infile.GetName())
	return setFile(infile.GetID(), partitionFile, infile.GetDB())
}

func storeEvidenceFile(infile structs.InputFile) error { return nil }

func evidenceFilePreflight(ehash []byte, name string, db *badger.DB) (structs.EvidenceFile, error) {
	var evidenceFile structs.EvidenceFile
	return evidenceFile, nil
}
