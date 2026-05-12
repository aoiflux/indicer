package cli

import (
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/enrichment"
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
)

func StoreData(chonkSize int, dbpath, evipath string, key []byte, noIndex bool, enableFTS bool, enableEnrichment bool) error {
	db, dbpath, err := Common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}
	err = util.EnsureBlobPath(dbpath)
	if err != nil {
		return err
	}

	finfo, err := os.Stat(evipath)
	if err != nil {
		return err
	}

	if finfo.IsDir() {
		fmt.Println("Storing Entire Folder")
		err = StoreFolder(chonkSize, evipath, key, noIndex, enableFTS, enableEnrichment, db)
		if err != nil {
			return err
		}
	}
	err = StoreFile(chonkSize, evipath, key, noIndex, enableFTS, enableEnrichment, db)
	if err != nil {
		return err
	}

	err = db.Close()
	if err != nil {
		return err
	}
	return nil
}

func StoreFolder(chonkSize int, evidir string, key []byte, noIndex bool, enableFTS bool, enableEnrichment bool, db *badger.DB) error {
	start := time.Now()

	err := filepath.Walk(evidir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		return StoreFile(chonkSize, path, key, noIndex, enableFTS, enableEnrichment, db)
	})

	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("Folder Store Time: ", time.Since(start))
	return nil
}

func StoreFile(chonkSize int, evipath string, key []byte, noIndex bool, enableFTS bool, enableEnrichment bool, db *badger.DB) error {
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
		if enableEnrichment {
			if err := enrichEvidenceNode(db, eviFile, evipath); err != nil {
				return err
			}
		}
		return nil
	}

	if !noIndex {
		if err = indexEvidenceFile(eviFile, db, enableFTS, enableEnrichment); err != nil {
			return err
		}
	}

	eviname := filepath.Base(evipath)
	fmt.Printf("\nSaving Evidence File: %s\n", eviname)

	echan := make(chan error)
	go store.Store(eviFile, echan)
	err = <-echan
	if err != nil {
		return err
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

	if enableEnrichment {
		if err := enrichEvidenceNode(db, eviFile, evipath); err != nil {
			return err
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
	fmt.Printf("\nStored in: %v\n\n", time.Since(start))
	return nil
}

func enrichEvidenceNode(db *badger.DB, eviFile structs.InputFile, evipath string) error {
	repo, err := enrichment.OpenGrapheneRepository(db.Opts().Dir)
	if err != nil {
		return err
	}
	service := enrichment.NewService(db, repo)
	defer service.Close()

	evidenceHashB64, err := eviFile.GetEncodedHash()
	if err != nil {
		return err
	}

	return service.EnrichEvidence(enrichment.EvidenceRecord{
		HashBase64: string(evidenceHashB64),
		Name:       filepath.Base(evipath),
		Path:       evipath,
		Size:       eviFile.GetSize(),
	})
}

func indexEvidenceFile(eviFile structs.InputFile, db *badger.DB, enableFTS bool, enableEnrichment bool) error {
	tuskJSON, hasTusk := parser.TuskAnalysis(eviFile.GetHandle().Name())
	partitions := parser.ParseImage(tuskJSON, hasTusk, eviFile.GetSize(), eviFile.GetHandle())
	idxChan := make(chan error)
	for index, partition := range partitions {
		phash := eviFile.GetHash()
		var err error
		if partition.Start != 0 && partition.Size != eviFile.GetSize() {
			phash, err = util.GetLogicalFileHash(eviFile.GetHandle(), cnst.GetHashAlgo(true), partition.Start, partition.Size, true)
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

		if hasTusk {
			go parser.IndexFilesystem(tuskJSON, pfile, idxChan, enableFTS, enableEnrichment)
		} else {
			go parser.IndexEXFAT(pfile, idxChan, enableFTS, enableEnrichment)
		}
		// Use select so that if the goroutine finishes before we send the
		// start signal (e.g. empty partition with no files), we receive the
		// result directly instead of deadlocking on the send.
		select {
		case idxChan <- nil:
			err = <-idxChan
		case err = <-idxChan:
		}
		if errors.Is(err, cnst.ErrIncompatibleFile) {
			continue
		}
		if err != nil {
			return err
		}
	}
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

	eviFileHash, err := util.GetFileHash(eviHandle, cnst.GetHashAlgo(true))
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
