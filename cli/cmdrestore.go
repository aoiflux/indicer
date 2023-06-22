package cli

import (
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/store"
	"os"
	"time"

	"github.com/ibraimgm/libcmd"
)

func RestoreData(cmd *libcmd.Cmd) error {
	start := time.Now()

	db, err := common(cmd)
	if err != nil {
		return err
	}
	fhash := cmd.Operand(cnst.OperandHash)
	if fhash == "" {
		return cnst.ErrHashNotFound
	}
	fpath := cmd.GetString(cnst.FlagRestoreFilePath)

	fhandle, err := os.Create(*fpath)
	if err != nil {
		return err
	}

	fmt.Println("Restoring file ...")
	err = store.Restore(fhash, fhandle, db)
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
