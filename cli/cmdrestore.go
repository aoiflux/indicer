package cli

import (
	"fmt"
	"indicer/lib/store"
	"indicer/lib/util"
	"os"
	"time"
)

func RestoreData(chonkSize int, dbpath, rhash, rpath string) error {
	start := time.Now()

	db, err := common(true, chonkSize, dbpath)
	if err != nil {
		return err
	}

	fhandle, err := os.Create(rpath)
	if err != nil {
		return err
	}

	fmt.Println("Restoring file ...")
	err = store.Restore(rhash, fhandle, db)
	if err != nil {
		return err
	}

	err = db.Close()
	if err != nil {
		return err
	}
	err = util.CompressDB(dbpath)
	if err != nil {
		return err
	}

	fmt.Println("Restored in: ", time.Since(start))
	return nil
}
