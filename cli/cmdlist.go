package cli

import (
	"indicer/lib/store"

	"github.com/ibraimgm/libcmd"
)

func ListData(cmd *libcmd.Cmd) error {
	db, err := common(cmd)
	if err != nil {
		return err
	}
	err = store.List(db)
	if err != nil {
		return err
	}
	return db.Close()
}
