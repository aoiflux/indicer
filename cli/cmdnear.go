package cli

import (
	"indicer/lib/near"
)

func NearInData(deep bool, chonkSize int, dbpath, inhash string, key []byte) error {
	db, _, err := Common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}
	return near.NearInFile(inhash, db, deep)
}

func NearOutData(chonkSize int, dbpath, outpath string, key []byte) error {
	db, _, err := Common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}
	err = near.NearOutFile(outpath, db)
	if err != nil {
		return err
	}
	return db.Close()
}
