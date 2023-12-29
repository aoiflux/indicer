package cli

import (
	"indicer/lib/search"
	"strings"
)

func SearchCmd(chonkSize int, query, dbpath string, key []byte) error {
	db, err := common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}
	query = strings.ToLower(query)
	return search.Search(query, db)
}
