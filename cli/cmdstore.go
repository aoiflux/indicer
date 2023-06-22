package cli

import (
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
	"github.com/ibraimgm/libcmd"
)

func StoreData(cmd *libcmd.Cmd) error {
	start := time.Now()

	db, err := common(cmd)
	if err != nil {
		return err
	}

	file := cmd.Operand(cnst.OperandFile)
	if file == "" {
		return cnst.ErrFileNotFound
	}

	fmt.Println("Pre-store checks & indexing....")
	eviFile, err := initEvidenceFile(file, db)
	if err != nil {
		return err
	}

	err = store.EvidenceFilePreStoreCheck(eviFile)
	if err == nil {
		fmt.Println("Evidence Store Time: ", time.Since(start))
		return nil
	}

	partitions := parser.GetPartitions(eviFile.GetSize(), eviFile.GetHandle())
	for index, partition := range partitions {
		phash, err := util.GetLogicalFileHash(eviFile.GetHandle(), partition.Start, partition.Size)
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
		if err != nil {
			return err
		}
	}

	echan := make(chan error)
	go store.Store(eviFile, echan)
	if <-echan != nil {
		return <-echan
	}

	err = eviFile.GetHandle().Close()
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

	eviFile = structs.NewInputFile(
		db,
		eviHandle,
		eviFileName,
		cnst.EviFileNamespace,
		eviFileHash,
		eviSize,
		0,
	)

	return eviFile, nil
}
