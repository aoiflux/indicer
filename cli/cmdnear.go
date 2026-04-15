package cli

import (
	"indicer/lib/near"
)

func NearInData(deep, verify bool, topK, chonkSize int, dbpath, inhash string, key []byte) error {
	db, _, err := Common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}
	defer db.Close()
	err = near.NearInFile(inhash, db, deep, verify, topK)
	if err != nil {
		return err
	}
	return nil
}

func NearOutData(deep, explainExact, verify bool, topK, chonkSize int, dbpath, outpath string, key []byte) error {
	db, _, err := Common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}
	defer db.Close()
	err = near.NearOutFile(outpath, db, deep, explainExact, verify, topK)
	if err != nil {
		return err
	}
	return nil
}
