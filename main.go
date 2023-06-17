package main

import (
	"errors"
	"fmt"
	"indicer/lib/cnst"
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
	fmt.Println("Initialising indicer....")

	if len(os.Args) < 2 {
		fmt.Println("indicer <store|list|restore|near|reset>")
		os.Exit(1)
	}

	dbpath, err := util.GetDBPath()
	handle(nil, nil, err)

	command := strings.ToLower(os.Args[1])

	var db *badger.DB
	if command != cnst.CmdReset {
		key := util.GetPassword()
		db, err = dbio.ConnectDB(dbpath, key)
		handle(nil, db, err)
		defer db.Close()
	}

	switch command {
	case cnst.CmdStore:
		err = storeData(db)
	case cnst.CmdList:
		err = listData(db)
	case cnst.CmdRestore:
		err = restoreData(db)
	case cnst.CmdNear:
		err = nearData(db)
	case cnst.CmdReset:
		err = resetData(dbpath)
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
	mapped := eviFile.GetMappedFile()
	defer mapped.Unmap()
	handle(mapped, db, err)

	err = store.EvidenceFilePreStoreCheck(eviFile)
	if err == nil {
		fmt.Println("Evidence Store Time: ", time.Since(start))
		return nil
	}

	partitions := parser.GetPartitions(eviFile.GetHandle(), eviFile.GetSize())
	for index, partition := range partitions {
		phash, err := util.GetLogicalFileHash(eviFile.GetHandle(), partition.Start, partition.Size)
		handle(mapped, db, err)
		eviFile.UpdateInternalObjects(partition.Start, partition.Size, phash)

		ehash, err := eviFile.GetEncodedHash()
		if err != nil {
			return err
		}

		pname := string(util.AppendToBytesSlice(ehash, cnst.DataSeperator, cnst.PartitionIndexPrefix, index))

		pfile := structs.NewInputFile(
			db,
			eviFile.GetHandle(),
			mapped,
			pname,
			cnst.PartiFileNamespace,
			phash,
			partition.Size,
			partition.Start,
		)

		err = parser.IndexEXFAT(db, pfile)
		if err == cnst.ErrIncompatibleFileSystem {
			fmt.Println(err, "...continuing")
			continue
		}
		handle(mapped, db, err)
	}

	echan := make(chan error)
	go store.Store(eviFile, echan)
	if <-echan != nil {
		return <-echan
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
		cnst.EviFileNamespace,
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
		return cnst.ErrIncorrectOption
	}

	inoption := strings.ToLower(os.Args[2])
	suboption := os.Args[3]

	var err error

	switch inoption {
	case cnst.InOptionIn:
		err = near.NearInFile(suboption, db)
	case cnst.InOptionOut:
		err = near.NearOutFile(suboption, db)
	default:
		return cnst.ErrIncorrectOption
	}

	return err
}

func resetData(dbpath string) error {
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
