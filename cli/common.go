package cli

import (
	"indicer/lib/dbio"
	"indicer/lib/util"

	"github.com/klauspost/compress/s2"
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
	err = util.DecompressDB(dbpath)
	if err != nil && err != s2.ErrCorrupt {
		return nil, err
	}
	util.SetChonkSize(chonkSize)
	return dbio.ConnectDB(readOnly, dbpath)
}
