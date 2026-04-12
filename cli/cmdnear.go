package cli

import (
	"indicer/lib/near"
)

func NearInData(deep bool, chonkSize int, dbpath, inhash string, key []byte) error {
	db, _, err := Common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}
	defer db.Close()
	err = near.NearInFile(inhash, db, deep)
	if err != nil {
		return err
	}
	return nil
}

func NearOutData(deep, explainExact bool, chonkSize int, dbpath, outpath string, key []byte) error {
	db, _, err := Common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}
	defer db.Close()
	err = near.NearOutFile(outpath, db, deep, explainExact)
	if err != nil {
		return err
	}
	return nil
}
