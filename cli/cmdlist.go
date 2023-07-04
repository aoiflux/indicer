package cli

import (
	"indicer/lib/store"
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
	return db.Close()
}
