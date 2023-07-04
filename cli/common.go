package cli

import (
	"indicer/lib/dbio"
	"indicer/lib/util"

	"go.etcd.io/bbolt"
)

func common(readOnly bool, chonkSize int, dbpath string) (*bbolt.DB, error) {
	var err error
	if dbpath == "" {
		dbpath, err = util.GetDBPath()
		if err != nil {
			return nil, err
		}
	}
	util.SetChonkSize(chonkSize)
	return dbio.ConnectDB(readOnly, dbpath)
}
