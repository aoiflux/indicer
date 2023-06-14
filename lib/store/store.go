package store

import (
	"bytes"
	"fmt"
	"indicer/lib/constant"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"strings"

	"github.com/dgraph-io/badger/v3"
	"github.com/schollz/progressbar/v3"
	"golang.org/x/exp/slices"
)

func Store(infile structs.InputFile, errchan chan error) {
	names := strings.Split(infile.GetName(), constant.FilePathSeperator)
	name := names[len(names)-1]

	if bytes.HasPrefix(infile.GetID(), []byte(constant.IndexedFileNamespace)) {
		fmt.Println("Saving indexed file: ", name)
		errchan <- storeIndexedFile(infile)
	} else if bytes.HasPrefix(infile.GetID(), []byte(constant.PartitionFileNamespace)) {
		fmt.Println("Saving partition file: ", name)
		errchan <- storePartitionFile(infile)
	} else {
		fmt.Println("Saving evidence file: ", name)
		errchan <- storeEvidenceFile(infile)
	}
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
	batch.SetMaxPendingTxns(constant.MaxBatchCount)
	var buffSize int64
	var active int

	bar := progressbar.DefaultBytes(infile.GetSize())
	fmt.Printf("\nSaving Evidence File\n")

	var tio structs.ThreadIO
	tio.FHash = infile.GetHash()
	tio.DB = infile.GetDB()
	tio.Batch = batch
	tio.Err = make(chan error, constant.MaxThreadCount)
	tio.MappedFile = infile.GetMappedFile()

	for storeIndex := infile.GetStartIndex(); storeIndex <= infile.GetSize(); storeIndex += constant.ChonkSize {
		tio.Index = storeIndex

		if infile.GetSize()-storeIndex <= constant.ChonkSize {
			buffSize = infile.GetSize() - storeIndex
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
	err = processRel(tio.Index, tio.FHash, chash, tio.DB, tio.Batch)
	if err != nil {
		tio.Err <- err
	}

	tio.Err <- processRevRel(tio.Index, tio.FHash, chash, tio.DB)
}
func processChonk(cdata, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	ckey := append([]byte(constant.ChonkNamespace), chash...)

	err := dbio.PingNode(ckey, db)
	if err != nil && err == badger.ErrKeyNotFound {
		return dbio.SetBatchNode(ckey, cdata, batch)
	}

	return err
}
func processRel(index int64, fhash, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	relKey := util.AppendToBytesSlice(constant.RelationNapespace, fhash, constant.PipeSeperator, index)

	err := dbio.PingNode(relKey, db)
	if err != nil && err == badger.ErrKeyNotFound {
		return dbio.SetBatchNode(relKey, chash, batch)
	}

	return err
}
func processRevRel(index int64, fhash, chash []byte, db *badger.DB) error {
	relVal := util.AppendToBytesSlice(constant.RelationNapespace, fhash, constant.PipeSeperator, index)
	revRelKey := util.AppendToBytesSlice(constant.ReverseRelationNamespace, chash)

	revRelList, err := dbio.GetReverseRelationNode(revRelKey, db)
	revRelNode := structs.ReverseRelation{Value: relVal}
	if err != nil && err == badger.ErrKeyNotFound {
		return dbio.SetReverseRelationNode(revRelKey, []structs.ReverseRelation{revRelNode}, db)
	}

	revRelList = append(revRelList, revRelNode)
	return dbio.SetReverseRelationNode(revRelKey, revRelList, db)
}
