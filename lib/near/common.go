package near

import (
	"bytes"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"strings"

	"github.com/dgraph-io/badger/v4"
)

func GetNames(id []byte, db *badger.DB, unique ...bool) (map[string]struct{}, error) {
	var uniqueFlag bool
	if len(unique) > 0 {
		uniqueFlag = unique[0]
	}

	if bytes.HasPrefix(id, []byte(cnst.IdxFileNamespace)) {
		ifile, err := dbio.GetIndexedFile(id, db)
		if uniqueFlag {
			return getUniqueNames(ifile.Names), err
		}
		return ifile.Names, err
	}

	if bytes.HasPrefix(id, []byte(cnst.PartiFileNamespace)) {
		pfile, err := dbio.GetPartitionFile(id, db)
		if uniqueFlag {
			return getUniqueNames(pfile.Names), err
		}
		return pfile.Names, err
	}

	efile, err := dbio.GetEvidenceFile(id, db)
	if err != nil {
		return nil, err
	}
	return efile.Names, err
}

func getUniqueNames(names map[string]struct{}) map[string]struct{} {
	uniqueMap := make(map[string]struct{})
	interimMap := make(map[string]string)

	for name := range names {
		split := strings.Split(name, cnst.DataSeperator)
		val := split[len(split)-1]

		split = split[:len(split)-1]
		key := strings.Join(split, cnst.DataSeperator)
		interimMap[key] = val
		delete(names, name)
	}

	for inKey, inVal := range interimMap {
		fullName := inKey + cnst.DataSeperator + inVal
		uniqueMap[fullName] = struct{}{}
		delete(interimMap, inKey)
	}

	return uniqueMap
}
