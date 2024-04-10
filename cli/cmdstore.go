package cli

import (
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/parser"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/edsrzf/mmap-go"
	"golang.org/x/crypto/sha3"
)

func StoreData(chonkSize int, dbpath, evidir string, key []byte, syncIndex, noIndex, folderStore bool) error {
	db, err := common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}
	err = util.EnsureBlobPath(dbpath)
	if err != nil {
		return err
	}

	if folderStore {
		fmt.Println("Storing Entire Folder")
		err = StoreFolder(chonkSize, evidir, key, syncIndex, noIndex, db)
		if err != nil {
			return err
		}
	}

	err = StoreFile(chonkSize, evidir, key, syncIndex, noIndex, db)
	if err != nil {
		return err
	}

	err = db.Close()
	if err != nil {
		return err
	}
	return nil
}

func StoreFolder(chonkSize int, evidir string, key []byte, syncIndex, noIndex bool, db *badger.DB) error {
	start := time.Now()

	err := filepath.Walk(evidir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		return StoreFile(chonkSize, path, key, syncIndex, noIndex, db)
	})

	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("Folder Store Time: ", time.Since(start))
	return nil
}

func StoreFile(chonkSize int, evipath string, key []byte, syncIndex, noIndex bool, db *badger.DB) error {
	start := time.Now()

	info, err := os.Stat(evipath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}

	fmt.Println("Pre-store checks....")
	eviFile, err := initEvidenceFile(evipath, db)
	if err != nil {
		return err
	}
	err = store.EvidenceFilePreStoreCheck(eviFile)
	if err != nil && err != badger.ErrKeyNotFound && err != cnst.ErrIncompleteFile {
		return err
	}
	if err == nil {
		return nil
	}

	var active int
	idxChan := make(chan error)
	if !noIndex {
		partitions := parser.GetPartitions(eviFile.GetSize(), eviFile.GetHandle())
		// not limiting goroutines here because max number of partitions will be 4 or less
		for index, partition := range partitions {
			phash := eviFile.GetHash()
			if partition.Start != 0 && partition.Size != eviFile.GetSize() {
				phash, err = util.GetLogicalFileHash(eviFile.GetHandle(), sha3.New256(), partition.Start, partition.Size, true)
				if err != nil {
					return err
				}
			}
			eviFile.UpdateInternalObjects(partition.Start, partition.Size, phash)

			ehash, err := eviFile.GetEncodedHash()
			if err != nil {
				return err
			}
			pname := string(util.AppendToBytesSlice(ehash, cnst.DataSeperator, eviFile.GetName(), "_", cnst.PartitionIndexPrefix, index))
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

			go parser.IndexEXFAT(pfile, idxChan)
			if !syncIndex {
				active++
			}
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
	}

	fmt.Printf("\nSaving Evidence File\n")
	if !syncIndex && active > 0 {
		fmt.Println("(indexer running async)")
	}

	echan := make(chan error)
	go store.Store(eviFile, echan)
	err = <-echan
	if err != nil {
		return err
	}

	if !syncIndex {
		for active > 0 {

			select {
			case err = <-idxChan:
				if err != nil && err != cnst.ErrIncompatibleFileSystem {
					return err
				}
			default:
				fmt.Println()
				idxChan <- nil
				err = <-idxChan
				if err != nil && err != cnst.ErrIncompatibleFileSystem {
					return err
				}
			}

			active--
		}
	}

	eviNode, err := dbio.GetEvidenceFile(eviFile.GetID(), eviFile.GetDB())
	if err != nil {
		return err
	}
	eviNode.Completed = true
	err = dbio.SetFile(eviFile.GetID(), eviNode, eviFile.GetDB())
	if err != nil {
		return err
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
	fmt.Printf("\nStored in: %v\n", time.Since(start))
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
	eviFileHash, err := util.GetFileHash(eviHandle, sha3.New256())
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
