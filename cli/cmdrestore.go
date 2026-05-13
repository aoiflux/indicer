package cli

import (
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/store"
	"os"
	"time"
)

func RestoreData(chonkSize int, dbpath, rhash, rpath string, key []byte) error {
	start := time.Now()

	db, _, err := Common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}

	fhandle, err := os.OpenFile(rpath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, cnst.FilePerm)
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
