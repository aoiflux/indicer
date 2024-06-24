package cli

import (
	"fmt"
	"indicer/lib/store"
	"os"
	"time"
)

func RestoreData(chonkSize int, dbpath, rhash, rpath string, key []byte) error {
	start := time.Now()

	db, _, err := common(chonkSize, dbpath, key)
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
	fmt.Println("Restored in: ", time.Since(start))
	return nil
}
