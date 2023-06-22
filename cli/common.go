package cli

import (
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v3"
	"github.com/ibraimgm/libcmd"
)

func common(cmd *libcmd.Cmd) (*badger.DB, error) {
	var err error

	dbpath := cmd.GetString(cnst.FlagDBPath)
	if *dbpath == "" {
		*dbpath, err = util.GetDBPath()
		if err != nil {
			return nil, err
		}
	}
	pwd := cmd.GetString(cnst.FlagPassword)
	key := util.HashPassword(*pwd)

	chonkSize := cmd.GetInt32(cnst.FlagChonkSize)
	util.SetChonkSize(int(*chonkSize))

	return dbio.ConnectDB(*dbpath, key)
}
