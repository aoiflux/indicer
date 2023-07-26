package cli

import (
	"indicer/lib/store"
)

func ListData(chonkSize int, dbpath string, key []byte) error {
	db, err := common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}
	err = store.List(db)
	if err != nil {
		return err
	}
	return db.Close()
}
