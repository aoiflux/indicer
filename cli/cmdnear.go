package cli

import (
	"indicer/lib/near"
	"indicer/lib/util"
)

func NearInData(deep bool, chonkSize int, dbpath, inhash string) error {
	db, err := common(true, chonkSize, dbpath)
	if err != nil {
		return err
	}
	err = near.NearInFile(inhash, db, deep)
	if err != nil {
		return err
	}
	err = db.Close()
	if err != nil {
		return err
	}
	return util.CompressDB(dbpath)
}

func NearOutData(chonkSize int, dbpath, outpath string) error {
	db, err := common(true, chonkSize, dbpath)
	if err != nil {
		return err
	}
	err = near.NearOutFile(outpath, db)
	if err != nil {
		return err
	}
	err = db.Close()
	if err != nil {
		return err
	}
	return util.CompressDB(dbpath)
}
