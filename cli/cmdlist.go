package cli

import (
	"indicer/lib/store"
	"indicer/lib/util"
)

func ListData(chonkSize int, dbpath string) error {
	db, err := common(true, chonkSize, dbpath)
	if err != nil {
		return err
	}
	err = store.List(db)
	if err != nil {
		return err
	}
	err = db.Close()
	if err != nil {
		return err
	}
	return util.CompressDB(dbpath)
}
