package cli

import (
	"indicer/lib/cnst"
	"indicer/lib/search"
	"strings"
)

func SearchCmd(chonkSize int, query, dbpath string, key []byte) error {
	if len(query) < 2 {
		return cnst.ErrSmallQuery
	}
	db, _, err := common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}
	query = strings.ToLower(query)
	return search.Search(query, db)
}
