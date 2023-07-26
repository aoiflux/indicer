package cli

import (
	"indicer/lib/dbio"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v3"
)

func common(chonkSize int, dbpath string, key []byte) (*badger.DB, error) {
	var err error
	if dbpath == "" {
		dbpath, err = util.GetDBPath()
		if err != nil {
			return nil, err
		}
	}
	util.SetChonkSize(chonkSize)
	return dbio.ConnectDB(dbpath, key)
}
