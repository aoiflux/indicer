package store

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"strings"
	"sync"

	"github.com/dgraph-io/badger/v3"
	"github.com/schollz/progressbar/v3"
	"golang.org/x/exp/slices"
)

func Store(infile structs.InputFile, errchan chan error) {
	names := strings.Split(infile.GetName(), cnst.DataSeperator)
	name := names[len(names)-1]

	if bytes.HasPrefix(infile.GetID(), []byte(cnst.IdxFileNamespace)) {
		fmt.Println("Saving indexed file: ", name)
		errchan <- storeIndexedFile(infile)
	} else if bytes.HasPrefix(infile.GetID(), []byte(cnst.PartiFileNamespace)) {
		fmt.Println("Saving partition file: ", name)
		errchan <- storePartitionFile(infile)
	} else {
		fmt.Println("Saving evidence file: ", name)
		errchan <- storeEvidenceFile(infile)
	}
}
func EvidenceFilePreStoreCheck(infile structs.InputFile) error {
	evidenceFile, err := dbio.GetEvidenceFile(infile.GetID(), infile.GetDB())
	if err != nil {
		return err
	}
	if !evidenceFile.Completed {
		return cnst.ErrIncompleteFile
	}
	if slices.Contains(evidenceFile.Names, infile.GetName()) {
		return nil
	}
	evidenceFile.Names = append(evidenceFile.Names, infile.GetName())
	return dbio.SetFile(infile.GetID(), evidenceFile, infile.GetDB())
}

func storeIndexedFile(infile structs.InputFile) error {
	indexedFile, err := dbio.GetIndexedFile(infile.GetID(), infile.GetDB())
	if err != nil && errors.Is(err, badger.ErrKeyNotFound) {
		indexedFile = structs.NewIndexedFile(
			infile.GetName(),
			infile.GetStartIndex(),
			infile.GetSize(),
		)
		return dbio.SetFile(infile.GetID(), indexedFile, infile.GetDB())
	}
	if err != nil && err != badger.ErrKeyNotFound {
		return err
	}

	if slices.Contains(indexedFile.Names, infile.GetName()) {
		return nil
	}

	indexedFile.Names = append(indexedFile.Names, infile.GetName())
	return dbio.SetFile(infile.GetID(), indexedFile, infile.GetDB())
}
func storePartitionFile(infile structs.InputFile) error {
	partitionFile, err := dbio.GetPartitionFile(infile.GetID(), infile.GetDB())
	if err != nil && errors.Is(err, badger.ErrKeyNotFound) {
		partitionFile = structs.NewPartitionFile(
			infile.GetName(),
			infile.GetStartIndex(),
			infile.GetSize(),
			infile.GetInternalObjects(),
		)
		return dbio.SetFile(infile.GetID(), partitionFile, infile.GetDB())
	}
	if err != nil && err != badger.ErrKeyNotFound {
		return err
	}

	if slices.Contains(partitionFile.Names, infile.GetName()) {
		return nil
	}

	partitionFile.Names = append(partitionFile.Names, infile.GetName())
	return dbio.SetFile(infile.GetID(), partitionFile, infile.GetDB())
}

func storeEvidenceFile(infile structs.InputFile) error {
	evidenceFile, err := evidenceFilePreflight(infile)
	if err != nil {
		return err
	}
	if evidenceFile.Completed {
		return nil
	}

	err = storeEvidenceData(infile)
	if err != nil {
		return err
	}

	evidenceFile.Completed = true
	return dbio.SetFile(infile.GetID(), evidenceFile, infile.GetDB())
}
func evidenceFilePreflight(infile structs.InputFile) (structs.EvidenceFile, error) {
	evidenceFile, err := dbio.GetEvidenceFile(infile.GetID(), infile.GetDB())
	if err != nil && errors.Is(err, badger.ErrKeyNotFound) {
		evidenceFile := structs.NewEvidenceFile(
			infile.GetName(),
			infile.GetStartIndex(),
			infile.GetSize(),
			infile.GetInternalObjects(),
		)
		err = dbio.SetFile(infile.GetID(), evidenceFile, infile.GetDB())
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
	err = dbio.SetFile(infile.GetID(), evidenceFile, infile.GetDB())
	return evidenceFile, err
}
func storeEvidenceData(infile structs.InputFile) error {
	fmt.Printf("\nSaving Evidence File\n")
	bar := progressbar.DefaultBytes(infile.GetSize())

	var tio structs.ThreadIO
	tio.FHash = infile.GetHash()
	tio.DB = infile.GetDB()

	count, err := cnst.GetMaxBatchCount()
	if err != nil {
		return err
	}
	tio.Batch = infile.GetDB().NewWriteBatch()
	tio.Batch.SetMaxPendingTxns(count)

	tio.MappedFile = infile.GetMappedFile()

	semaphore := make(chan struct{}, cnst.GetMaxThreadCount())
	var wg sync.WaitGroup

	for storeIndex := infile.GetStartIndex(); storeIndex <= infile.GetSize(); storeIndex += cnst.ChonkSize {
		tio.Index = storeIndex
		tio.ChonkEnd = storeIndex + cnst.ChonkSize
		if tio.ChonkEnd > infile.GetSize() {
			tio.ChonkEnd = infile.GetSize()
		}

		semaphore <- struct{}{}
		wg.Add(1)
		go func(tio structs.ThreadIO) error {
			defer func() {
				<-semaphore
				wg.Done()
			}()
			err = storeWorker(tio)
			if err != nil {
				return err
			}
			bar.Add64(tio.ChonkEnd - tio.Index)
			return nil
		}(tio)
	}

	wg.Wait()
	defer tio.Batch.Flush()

	bar.Finish()
	return nil
}

func storeWorker(tio structs.ThreadIO) error {
	lostChonk := tio.MappedFile[tio.Index:tio.ChonkEnd]
	chash, err := util.GetChonkHash(lostChonk)
	if err != nil {
		return err
	}

	err = processChonk(lostChonk, chash, tio.DB, tio.Batch)
	if err != nil {
		return err
	}

	err = processRel(tio.Index, tio.FHash, chash, tio.DB, tio.Batch)
	if err != nil {
		return err
	}

	return processRevRel(tio.Index, tio.FHash, chash, tio.DB, tio.Batch)
}

func processChonk(cdata, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
	return dbio.SetBatchNode(ckey, cdata, batch)
}

func processRel(index int64, fhash, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	relKey := util.AppendToBytesSlice(cnst.RelationNamespace, fhash, cnst.DataSeperator, index)
	return dbio.SetBatchNode(relKey, chash, batch)
}

func processRevRel(index int64, fhash, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	fhashStr := base64.StdEncoding.EncodeToString(fhash)
	relVal := util.AppendToBytesSlice(cnst.RelationNamespace, fhashStr, cnst.DataSeperator, index)
	revRelKey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash)

	revRelList, err := dbio.GetReverseRelationNode(revRelKey, db)
	if err != nil && !errors.Is(err, badger.ErrKeyNotFound) {
		return err
	}

	revRelNode := structs.ReverseRelation{Value: relVal}
	revRelList = append(revRelList, revRelNode)
	return dbio.SetReverseRelationNode(revRelKey, revRelList, batch)
}
