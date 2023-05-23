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
	batch := db.NewWriteBatch()
	batch.SetMaxPendingTxns(constant.MaxBatchCount)
	var buffSize int64
	var active int

	bar := progressbar.DefaultBytes(size)
	fmt.Printf("\nSaving File\n")

	var tio structs.ThreadIO
	tio.FHash = fhash
	tio.DB = db
	tio.Batch = batch
	tio.Err = make(chan error, constant.MaxThreadCount)
	tio.MappedFile = mappedFile

	for storeIndex := start; ; storeIndex += constant.ChonkSize {
		if storeIndex > size {
			break
		}
		tio.Index = storeIndex

		if size-storeIndex <= constant.ChonkSize {
			buffSize = size - storeIndex
		} else {
			buffSize = constant.ChonkSize
		}

		tio.End = storeIndex + buffSize
		go storeWorker(tio)
		active++

		if active > constant.MaxThreadCount {
			err := <-tio.Err
			if err != nil {
				return err
			}
			active--
			bar.Add64(buffSize)
		}
	}

	for active > 0 {
		err := <-tio.Err
		if err != nil {
			return err
		}
		active--
		bar.Add64(constant.ChonkSize)
	}

	err := tio.Batch.Flush()
	if err != nil {
		return err
	}

	bar.Add64(constant.ChonkSize)
	bar.Finish()
	return nil
}
func storeWorker(tio structs.ThreadIO) {
	lostChonk := tio.MappedFile[tio.Index:tio.End]

	chash, err := util.GetChonkHash(lostChonk)
	if err != nil {
		tio.Err <- err
	}
	err = processChonk(lostChonk, chash, tio.DB, tio.Batch)
	if err != nil {
		tio.Err <- err
	}

	tio.Err <- processRel(tio.Index, tio.FHash, chash, tio.DB, tio.Batch)
}
func processChonk(cdata, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	ckey := append([]byte(constant.ChonkNamespace), chash...)

	err := pingNode(ckey, db)
	if err != nil && err == badger.ErrKeyNotFound {
		return setBatchNode(ckey, cdata, batch)
	}

	return err
}
func processRel(index int64, fhash, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	relKey := util.AppendToBytesSlice(constant.RelationNapespace, fhash, constant.PipeSeperator, index)

	err := pingNode(relKey, db)
	if err != nil && err == badger.ErrKeyNotFound {
		return setBatchNode(relKey, chash, batch)
	}

	return err
}
