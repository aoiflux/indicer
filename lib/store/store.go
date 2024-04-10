package store

import (
	"bytes"
	"errors"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/schollz/progressbar/v3"
	"golang.org/x/crypto/sha3"
)

func Store(infile structs.InputFile, errchan chan error) {
	if bytes.HasPrefix(infile.GetID(), []byte(cnst.PartiFileNamespace)) {
		errchan <- storePartitionFile(infile)
	} else {
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
	if _, ok := evidenceFile.Names[infile.GetName()]; ok {
		return nil
	}

	evidenceFile.Names[infile.GetName()] = struct{}{}
	return dbio.SetFile(infile.GetID(), evidenceFile, infile.GetDB())
}

func storePartitionFile(infile structs.InputFile) error {
	partitionFile, err := dbio.GetPartitionFile(infile.GetID(), infile.GetDB())
	if errors.Is(err, badger.ErrKeyNotFound) {
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

	if _, ok := partitionFile.Names[infile.GetName()]; ok {
		return nil
	}

	partitionFile.Names[infile.GetName()] = struct{}{}
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
	return storeEvidenceData(infile)
}
func evidenceFilePreflight(infile structs.InputFile) (structs.EvidenceFile, error) {
	evidenceFile, err := dbio.GetEvidenceFile(infile.GetID(), infile.GetDB())
	if errors.Is(err, badger.ErrKeyNotFound) {
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
	if _, ok := evidenceFile.Names[infile.GetName()]; ok {
		return evidenceFile, nil
	}

	evidenceFile.Names[infile.GetName()] = struct{}{}
	err = dbio.SetFile(infile.GetID(), evidenceFile, infile.GetDB())
	return evidenceFile, err
}
func storeEvidenceData(infile structs.InputFile) error {
	bar := progressbar.DefaultBytes(infile.GetSize())

	var tio structs.ThreadIO
	tio.FHash = infile.GetHash()
	tio.DB = infile.GetDB()

	var err error
	tio.Batch, err = util.InitBatch(infile.GetDB())
	if err != nil {
		return err
	}

	tio.Err = make(chan error, cnst.GetMaxThreadCount())
	tio.MappedFile = infile.GetMappedFile()

	var active int
	var buffsize int64
	for storeIndex := infile.GetStartIndex(); storeIndex < infile.GetSize(); storeIndex += cnst.ChonkSize {
		tio.Index = storeIndex

		if infile.GetSize()-storeIndex <= cnst.ChonkSize {
			buffsize = infile.GetSize() - storeIndex
		} else {
			buffsize = cnst.ChonkSize
		}

		tio.ChonkEnd = tio.Index + buffsize
		go storeWorker(tio)
		active++

		if active > cnst.GetMaxThreadCount() {
			err := <-tio.Err
			if err != nil {
				return err
			}
			active--
			bar.Add64(buffsize)
		}
	}

	for active > 0 {
		err := <-tio.Err
		if err != nil {
			return err
		}
		active--
		bar.Add64(cnst.ChonkSize)
	}

	err = tio.Batch.Flush()
	if err != nil {
		return err
	}

	bar.Add64(cnst.ChonkSize)
	bar.Finish()
	return bar.Close()
}
func storeWorker(tio structs.ThreadIO) {
	lostChonk := tio.MappedFile[tio.Index:tio.ChonkEnd]
	chash, err := util.GetChonkHash(lostChonk, sha3.New512())
	if err != nil {
		tio.Err <- err
		return
	}
	err = processChonk(lostChonk, chash, tio.DB, tio.Batch)
	if err != nil {
		tio.Err <- err
		return
	}
	err = processRel(tio.Index, tio.FHash, chash, tio.DB, tio.Batch)
	if err != nil {
		tio.Err <- err
		return
	}
	tio.Err <- processRevRel(tio.Index, tio.FHash, chash, tio.DB, tio.Batch)
}
func processChonk(cdata, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)

	err := dbio.PingNode(ckey, db)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return dbio.SetBatchChonkNode(ckey, cdata, db, batch)
	}

	return err
}
func processRel(index int64, fhash, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	relKey := util.AppendToBytesSlice(cnst.RelationNamespace, fhash, cnst.DataSeperator, index)

	err := dbio.PingNode(relKey, db)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return dbio.SetBatchNode(relKey, chash, batch)
	}

	return err
}
func processRevRel(index int64, fhash, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	revRelKey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator, index)

	revRelMap, err := dbio.GetReverseRelationNode(revRelKey, db)
	if errors.Is(err, badger.ErrKeyNotFound) {
		revRelMap = make(map[string]struct{})
		revRelMap[string(fhash)] = struct{}{}
		return dbio.SetReverseRelationNode(revRelKey, revRelMap, batch)
	}
	if err != nil && err != badger.ErrKeyNotFound {
		return err
	}

	revRelMap[string(fhash)] = struct{}{}
	return dbio.SetReverseRelationNode(revRelKey, revRelMap, batch)
}
