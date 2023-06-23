package cli

import (
	"indicer/lib/near"
)

func NearInData(deep bool, chonkSize int, dbpath, pwd, inhash string) error {
	db, err := common(chonkSize, dbpath, pwd)
	if err != nil {
		return err
	}
	return near.NearInFile(inhash, db, deep)
}

func NearOutData(chonkSize int, dbpath, pwd, outpath string) error {
	db, err := common(chonkSize, dbpath, pwd)
	if err != nil {
		return err
	}
	err = near.NearOutFile(outpath, db)
	if err != nil {
		return err
	}
	return db.Close()
}
