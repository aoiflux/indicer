package store

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"strings"

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
	if err != nil && err == badger.ErrKeyNotFound {
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
	if err != nil && err == badger.ErrKeyNotFound {
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
	if err != nil && err == badger.ErrKeyNotFound {
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
	batch := infile.GetDB().NewWriteBatch()
	batch.SetMaxPendingTxns(cnst.MaxBatchCount)

	fmt.Printf("\nSaving Evidence File\n")
	bar := progressbar.DefaultBytes(infile.GetSize())

	var tio structs.ThreadIO
	tio.FHash = infile.GetHash()
	tio.DB = infile.GetDB()
	tio.Batch = batch
	tio.Err = make(chan error, cnst.GetMaxThreadCount())
	tio.FHandle = infile.GetHandle()

	var active int

	for storeIndex := infile.GetStartIndex(); storeIndex <= infile.GetSize(); storeIndex += cnst.ChonkSize {
		tio.Index = storeIndex

		if infile.GetSize()-storeIndex <= cnst.ChonkSize {
			tio.BuffSize = infile.GetSize() - storeIndex
		} else {
			tio.BuffSize = cnst.ChonkSize
		}

		go storeWorker(tio)
		active++

		if active > cnst.GetMaxThreadCount() {
			err := <-tio.Err
			if err != nil {
				return err
			}
			active--
			bar.Add64(tio.BuffSize)
		}

		tio.DB.RunValueLogGC(0.5)
	}

	for active > 0 {
		err := <-tio.Err
		if err != nil {
			return err
		}
		active--
		bar.Add64(cnst.ChonkSize)
	}

	err := tio.Batch.Flush()
	if err != nil {
		return err
	}

	bar.Add64(cnst.ChonkSize)
	bar.Finish()
	return nil
}
func storeWorker(tio structs.ThreadIO) {
	lostChonk := make([]byte, tio.BuffSize)
	_, err := tio.FHandle.ReadAt(lostChonk, tio.Index)
	if err != nil {
		tio.Err <- err
	}

	chash, err := util.GetChonkHash(lostChonk)
	if err != nil {
		tio.Err <- err
	}
	err = processChonk(lostChonk, chash, tio.DB, tio.Batch)
	if err != nil {
		tio.Err <- err
	}
	err = processRel(tio.Index, tio.FHash, chash, tio.DB, tio.Batch)
	if err != nil {
		tio.Err <- err
	}

	tio.Err <- processRevRel(tio.Index, tio.FHash, chash, tio.DB, tio.Batch)
}
func processChonk(cdata, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	ckey := append([]byte(cnst.ChonkNamespace), chash...)

	err := dbio.PingNode(ckey, db)
	if err != nil && err == badger.ErrKeyNotFound {
		return dbio.SetBatchNode(ckey, cdata, batch)
	}

	return err
}
func processRel(index int64, fhash, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	relKey := util.AppendToBytesSlice(cnst.RelationNamespace, fhash, cnst.DataSeperator, index)

	err := dbio.PingNode(relKey, db)
	if err != nil && err == badger.ErrKeyNotFound {
		return dbio.SetBatchNode(relKey, chash, batch)
	}

	return err
}
func processRevRel(index int64, fhash, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	fhashStr := base64.StdEncoding.EncodeToString(fhash)
	relVal := util.AppendToBytesSlice(cnst.RelationNamespace, fhashStr, cnst.DataSeperator, index)
	revRelKey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash)

	revRelList, err := dbio.GetReverseRelationNode(revRelKey, db)
	revRelNode := structs.ReverseRelation{Value: relVal}
	if err != nil && err == badger.ErrKeyNotFound {
		return dbio.SetReverseRelationNode(revRelKey, []structs.ReverseRelation{revRelNode}, batch)
	}

	revRelList = append(revRelList, revRelNode)
	return dbio.SetReverseRelationNode(revRelKey, revRelList, batch)
}
