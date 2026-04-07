package cli

import (
	"indicer/lib/store"
)

func StatsData(chonkSize int, dbpath string, key []byte) error {
	db, _, err := Common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}
	err = store.Stats(db)
	if err != nil {
		return err
	}
	return db.Close()
}
