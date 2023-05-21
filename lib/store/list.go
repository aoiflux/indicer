package store

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"indicer/lib/constant"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v3"
	"github.com/klauspost/compress/s2"
)

func ListFiles(db *badger.DB) error {
	return db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 1000
		it := txn.NewIterator(opts)
		defer it.Close()

		eviPrefix := []byte(constant.EvidenceFileNamespace)
		for it.Seek(eviPrefix); it.ValidForPrefix(eviPrefix); it.Next() {
			item := it.Item()
			k := item.KeyCopy(nil)
			v, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}

			decoded, err := s2.Decode(nil, v)
			if err != nil {
				return err
			}

			var evidata structs.EvidenceFile
			err = json.Unmarshal(decoded, &evidata)
			if err != nil {
				return err
			}

			if !evidata.Completed {
				continue
			}

			evihash := bytes.Split(k, eviPrefix)[1]
			fmt.Println(base64.StdEncoding.EncodeToString(evihash))
		}

		return nil
	})
}
