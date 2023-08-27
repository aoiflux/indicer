package cli

import (
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/parser"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"
	"path/filepath"
	"time"

	"github.com/dgraph-io/badger/v3"
	"github.com/edsrzf/mmap-go"
)

func StoreData(chonkSize int, dbpath, evipath string, key []byte, syncIndex bool) error {
	start := time.Now()

	db, err := common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}
	err = util.EnsureBlobPath(dbpath)
	if err != nil {
		return err
	}

	fmt.Println("Pre-store checks & indexing....")
	eviFile, err := initEvidenceFile(evipath, db)
	if err != nil {
		return err
	}

	// not limiting goroutines here because max number of partitions will be 4 or less
	idxChan := make(chan error)
	partitions := parser.GetPartitions(eviFile.GetSize(), eviFile.GetHandle())
	for index, partition := range partitions {
		phash, err := util.GetLogicalFileHash(eviFile.GetHandle(), partition.Start, partition.Size, true)
		if err != nil {
			return err
		}
		eviFile.UpdateInternalObjects(partition.Start, partition.Size, phash)

		ehash, err := eviFile.GetEncodedHash()
		if err != nil {
			return err
		}
		pname := string(util.AppendToBytesSlice(ehash, cnst.DataSeperator, cnst.PartitionIndexPrefix, index))
		pfile := structs.NewInputFile(
			db,
			eviFile.GetHandle(),
			eviFile.GetMappedFile(),
			pname,
			cnst.PartiFileNamespace,
			phash,
			partition.Size,
			partition.Start,
		)

		go parser.IndexEXFAT(db, pfile, idxChan)
		if syncIndex {
			idxChan <- nil
			err = <-idxChan
			if errors.Is(err, cnst.ErrIncompatibleFile) {
				continue
			}
			if err != nil {
				return err
			}
		}
		if err != nil {
			return err
		}
	}

	fmt.Printf("\nSaving Evidence File\n")
	if !syncIndex {
		fmt.Println("(indexer running async)")
	}

	echan := make(chan error)
	go store.Store(eviFile, echan)
	err = <-echan
	if err != nil {
		return err
	}

	if !syncIndex {
		select {
		case err, ok := <-idxChan:
			if ok {
				if err != nil {
					return err
				}
			} else {
				idxChan <- nil
				fmt.Println()
				err := <-idxChan
				if !errors.Is(err, cnst.ErrIncompatibleFileSystem) {
					return err
				}
			}
		default:
		}
	}

	mappedFile := eviFile.GetMappedFile()
	err = mappedFile.Unmap()
	if err != nil {
		return err
	}
	err = eviFile.GetHandle().Close()
	if err != nil {
		return err
	}
	err = eviFile.GetDB().Close()
	if err != nil {
		return err
	}
	fmt.Println("Stored in: ", time.Since(start))
	return nil
}

func initEvidenceFile(evifilepath string, db *badger.DB) (structs.InputFile, error) {
	var eviFile structs.InputFile

	eviInfo, err := os.Stat(evifilepath)
	if err != nil {
		return eviFile, err
	}
	eviSize := eviInfo.Size()
	eviHandle, err := os.Open(evifilepath)
	if err != nil {
		return eviFile, err
	}
	eviFileName := filepath.Base(evifilepath)
	eviFileHash, err := util.GetFileHash(eviHandle)
	if err != nil {
		return eviFile, err
	}

	mappedFile, err := mmap.Map(eviHandle, mmap.RDONLY, 0)
	if err != nil {
		return eviFile, err
	}

	eviFile = structs.NewInputFile(
		db,
		eviHandle,
		mappedFile,
		eviFileName,
		cnst.EviFileNamespace,
		eviFileHash,
		eviSize,
		0,
	)

	return eviFile, nil
}
