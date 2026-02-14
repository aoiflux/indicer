package cli

import (
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/fatih/color"
)

func common(chonkSize int, dbpath string, key []byte) (*badger.DB, string, error) {
	var err error
	if dbpath == "" {
		dbpath, err = util.GetDBPath()
		if err != nil {
			return nil, "", err
		}
	}
	util.SetChonkSize(chonkSize)

	// Recommend container mode for small chunk sizes
	if chonkSize < 128 && !cnst.CONTAINERMODE {
		color.Yellow("\n⚠️  RECOMMENDATION: You're using a chunk size of %dKB (< 128KB).", chonkSize)
		color.Yellow("   Consider using --container flag for better filesystem efficiency.")
		color.Yellow("   Container mode packs multiple small chunks into 1GB compressed files.\n")
		fmt.Println()
	}

	db, err := dbio.ConnectDB(dbpath, key)
	return db, dbpath, err
}
