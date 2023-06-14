package main

import (
	"errors"
	"fmt"
	"indicer/lib/constant"
	"indicer/lib/dbio"
	"indicer/lib/near"
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
	"github.com/fatih/color"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("indicer <store|list|restore|near|reset>")
		os.Exit(1)
	}

	dbpath, err := util.GetDBPath()
	handle(nil, nil, err)

	command := strings.ToLower(os.Args[1])

	var db *badger.DB
	if command != constant.CmdReset {
		// key := util.GetPassword()
		key := []byte{}
		db, err = dbio.ConnectDB(dbpath, key)
		handle(nil, db, err)
		defer db.Close()
	}

	switch command {
	case constant.CmdStore:
		err = storeData(db)
	case constant.CmdList:
		err = listData(db)
	case constant.CmdRestore:
		err = restoreData(db)
	case constant.CmdNear:
		err = nearData(db)
	case constant.CmdReset:
		err = resetDatabase(dbpath)
	}

	handle(nil, db, err)
}

func storeData(db *badger.DB) error {
	start := time.Now()

	if len(os.Args) < 3 {
		return errors.New("indicer store <src_file_path> [chonk_size_in_kb]")
	}

	if len(os.Args) > 3 {
		util.SetChonkSize(os.Args[3])
	}

	eviFile, err := initEvidenceFile(db, os.Args[2])
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

	if len(os.Args) < 4 {
		return errors.New("indicer restore <hash> <dstfilepath> [chonk_size_in_kb]")
	}

	fhandle, err := os.Create(os.Args[3])
	if err != nil {
		return err
	}

	if len(os.Args) > 5 {
		util.SetChonkSize(os.Args[4])
	}

	fmt.Println("Restoring file ...")
	err = store.Restore(os.Args[2], fhandle, db)
	if err != nil {
		return err
	}

	fmt.Println("Restored in: ", time.Since(start))
	return nil
}

func nearData(db *badger.DB) error {
	if len(os.Args) < 4 {
		return constant.ErrIncorrectOption
	}

	inoption := strings.ToLower(os.Args[2])
	suboption := os.Args[3]

	var err error

	switch inoption {
	case constant.InOptionIn:
		err = near.NearInFile(suboption, db)
	case constant.InOptionOut:
		err = near.NearOutFile(suboption, db)
	default:
		return constant.ErrIncorrectOption
	}

	return err
}

func resetDatabase(dbpath string) error {
	color.Red("WARNING! This command will DELETE ALL the saved files.")
	fmt.Printf("Are you sure about this? [y/N] ")

	var in string
	fmt.Scanln(&in)
	in = strings.ToLower(in)

	if in != "y" {
		color.Blue("Your data is SAFE!")
		return nil
	}

	color.Red("Deleting ALL data!")
	return os.RemoveAll(dbpath)
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
