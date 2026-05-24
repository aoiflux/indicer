package near

import (
	"bytes"
	"indicer/lib/cnst"
	"indicer/lib/dbio"

	"github.com/dgraph-io/badger/v4"
)

func GetNames(id []byte, db *badger.DB, unique ...bool) (map[string]struct{}, error) {
	if bytes.HasPrefix(id, []byte(cnst.IdxFileNamespace)) {
		ifile, err := dbio.GetIndexedFile(id, db)
		return singleNameSet(ifile.Name), err
	}

	if bytes.HasPrefix(id, []byte(cnst.PartiFileNamespace)) {
		pfile, err := dbio.GetPartitionFile(id, db)
		return singleNameSet(pfile.Name), err
	}

	efile, err := dbio.GetEvidenceFile(id, db)
	if err != nil {
		return nil, err
	}
	return singleNameSet(efile.Name), err
}

func singleNameSet(name string) map[string]struct{} {
	if name == "" {
		return nil
	}
	return map[string]struct{}{name: {}}
}
