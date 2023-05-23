package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"indicer/lib/constant"
	"indicer/lib/parser"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dgraph-io/badger/v3"
	"github.com/edsrzf/mmap-go"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("indicer <store|list|restore> <db_path>")
		os.Exit(1)
	}

	dbpath, err := util.GetDBPath()
	handle(nil, nil, err)

	key := util.GetPassword()
	db, err := store.ConnectDB(dbpath, key)
	handle(nil, db, err)

	switch strings.ToLower(os.Args[1]) {
	case constant.CmdStore:
		err = storeData(db)
	case constant.CmdList:
		err = listData(db)
	case constant.CmdRestore:
		err = restoreData(db)
	}
	handle(nil, db, err)

	err = db.Close()
	handle(nil, db, err)
}

func storeData(db *badger.DB) error {
	start := time.Now()

	if len(os.Args) < 4 {
		return errors.New("indicer store [db_path] <src_file_path> [chonk_size_in_kb]")
	}

	if len(os.Args) > 4 {
		util.SetChonkSize(os.Args[4])
	}

	eviFile, err := initEvidenceFile(db, os.Args[3])
	handle(eviFile.GetMappedFile(), db, err)

	partitions, err := parser.GetPartitions(eviFile.GetHandle(), eviFile.GetSize())
	handle(eviFile.GetMappedFile(), db, err)

	for index, partition := range partitions {
		phash, err := util.GetLogicalFileHash(eviFile.GetHandle(), partition.Start, partition.Size)
		handle(eviFile.GetMappedFile(), db, err)
		eviFile.UpdateInternalObjects(phash)

		ehash, err := eviFile.GetEncodedHash()
		if err != nil {
			return err
		}

		pname := string(util.AppendToBytesSlice(ehash, constant.FilePathSeperator, constant.PartitionIndexPrefix, index))

		pfile := structs.NewInputFile(
			db,
			eviFile.GetHandle(),
			eviFile.GetMappedFile(),
			pname,
			constant.PartitionFileNamespace,
			phash,
			partition.Size,
			partition.Start,
		)

		err = parser.IndexEXFAT(db, pfile)
		if err == constant.ErrIncompatibleFileSystem {
			fmt.Println(err, "...continuing")
			continue
		}
		handle(eviFile.GetMappedFile(), db, err)
	}

	echan := make(chan error)
	go store.Store(eviFile, echan)
	if <-echan != nil {
		return <-echan
	}

	mapped := eviFile.GetMappedFile()
	err = mapped.Unmap()
	if err != nil {
		return err
	}
	fmt.Println("Evidence Store Time: ", time.Since(start))
	return nil
}
func initEvidenceFile(db *badger.DB, evifilepath string) (structs.InputFile, error) {
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
		constant.EvidenceFileNamespace,
		eviFileHash,
		eviSize,
		0,
	)

	return eviFile, nil
}

func listData(db *badger.DB) error {
	return store.List(db)
}

func restoreData(db *badger.DB) error {
	start := time.Now()

	if len(os.Args) < 6 {
		return errors.New("indicer restore <db_path> <evidence|partition|indexed> <hash> <dstfilepath> [chonk_size_in_kb]")
	}

	fhandle, err := os.Create(os.Args[5])
	if err != nil {
		return err
	}

	fid, err := getRestoreFileID(os.Args[3], os.Args[4])
	if err != nil {
		return err
	}

	if len(os.Args) > 6 {
		util.SetChonkSize(os.Args[6])
	}

	fmt.Println("Restoring file ...")
	err = store.Restore(db, fhandle, fid)
	if err != nil {
		return err
	}

	fmt.Println("Restored in: ", time.Since(start))
	return nil
}

func getRestoreFileID(ftype, fhashString string) ([]byte, error) {
	fhash, err := base64.StdEncoding.DecodeString(fhashString)
	if err != nil {
		return nil, err
	}

	ftype = strings.ToLower(ftype)
	switch ftype {
	case constant.IndexedFileType:
		return append([]byte(constant.IndexedFileNamespace), fhash...), nil
	case constant.PartitionFileType:
		return append([]byte(constant.PartitionFileNamespace), fhash...), nil
	case constant.EvidenceFileType:
		return append([]byte(constant.EvidenceFileNamespace), fhash...), nil
	default:
		return nil, constant.ErrUnknownFileType
	}
}

func handle(mappedFile mmap.MMap, db *badger.DB, err error) {
	if err != nil {
		fmt.Printf("\n\n%v\n\n", err)

		if db != nil {
			db.Close()
		}

		if mappedFile != nil {
			mappedFile.Unmap()
		}

		os.Exit(1)
	}
}
