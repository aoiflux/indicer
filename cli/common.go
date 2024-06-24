package cli

import (
	"indicer/lib/dbio"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

func common(chonkSize int, dbpath string, key []byte) (*badger.DB, string, error) {
	var err error
	if dbpath == "" {
		dbpath, err = util.GetDBPath()
		if err != nil {
			return nil, "", err
		}
	}
	util.SetChonkSize(chonkSize)
	db, err := dbio.ConnectDB(dbpath, key)
	return db, dbpath, err
}
