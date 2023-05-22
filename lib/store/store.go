package store

import (
	"bytes"
	"fmt"
	"indicer/lib/constant"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v3"
	"github.com/edsrzf/mmap-go"
	"github.com/schollz/progressbar/v3"
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

func storeEvidenceFile(infile structs.InputFile) error {
	evidenceFile, err := evidenceFilePreflight(infile)
	if err != nil {
		return err
	}
	if evidenceFile.Completed {
		return nil
	}
	err = storeData(infile.GetMappedFile(), infile.GetStartIndex(), infile.GetSize(), infile.GetHash(), infile.GetDB())
	if err != nil {
		return err
	}
	evidenceFile.Completed = true
	return setFile(infile.GetID(), evidenceFile, infile.GetDB())
}

func evidenceFilePreflight(infile structs.InputFile) (structs.EvidenceFile, error) {
	evidenceFile, err := getEvidenceFile(infile.GetID(), infile.GetDB())
	if err != nil && err == badger.ErrKeyNotFound {
		evidenceFile := structs.NewEvidenceFile(
			infile.GetName(),
			infile.GetStartIndex(),
			infile.GetSize(),
			infile.GetInternalObjects(),
		)
		err = setFile(infile.GetID(), evidenceFile, infile.GetDB())
		return evidenceFile, err
	}
	if err != nil && err != badger.ErrKeyNotFound {
		return evidenceFile, err
	}

	if !evidenceFile.Completed {
		return evidenceFile, nil
	}
	if slices.Contains(evidenceFile.Names, infile.GetName()) {
		return evidenceFile, nil
	}

	evidenceFile.Names = append(evidenceFile.Names, infile.GetName())
	err = setFile(infile.GetID(), evidenceFile, infile.GetDB())
	return evidenceFile, err
}

func storeData(mappedFile mmap.MMap, start, size int64, fhash []byte, db *badger.DB) error {
	var buffSize int64
	bar := progressbar.DefaultBytes(size)
	fmt.Printf("\nSaving File\n")

	for storeIndex := start; ; storeIndex += constant.ChonkSize {
		if storeIndex > size {
			break
		}

		if size-storeIndex <= constant.ChonkSize {
			buffSize = size - storeIndex
		} else {
			buffSize = constant.ChonkSize
		}

		err := storeWorker(mappedFile, storeIndex, storeIndex+buffSize, fhash, db)
		if err != nil {
			return err
		}

		bar.Add64(buffSize)
	}

	bar.Add64(buffSize)
	bar.Finish()
	return nil
}

func storeWorker(mappedFile mmap.MMap, index, end int64, fhash []byte, db *badger.DB) error {
	lostChonk := mappedFile[index:end]

	chash, err := util.GetChonkHash(lostChonk)
	if err != nil {
		return err
	}
	ckey := append([]byte(constant.ChonkNamespace), chash...)
	err = pingNode(ckey, db)
	if err != nil && err == badger.ErrKeyNotFound {
		return setNode(ckey, lostChonk, db)
	}
	if err != nil && err != badger.ErrKeyNotFound {
		return err
	}

	relKey := util.AppendToBytesSlice(constant.RelationNapespace, fhash, constant.PipeSeperator, index)
	err = pingNode(relKey, db)
	if err != nil && err == badger.ErrKeyNotFound {
		return setNode(relKey, chash, db)
	}
	if err != nil && err != badger.ErrKeyNotFound {
		return err
	}

	return nil
}
