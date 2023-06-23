package cli

import (
	"indicer/lib/store"
)

func ListData(chonkSize int, dbpath, pwd string) error {
	db, err := common(chonkSize, dbpath, pwd)
	if err != nil {
		return err
	}
	err = store.List(db)
	if err != nil {
		return err
	}
	return db.Close()
}
