package store

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/structs"
	"strings"

	"github.com/dgraph-io/badger/v4"
	"github.com/dustin/go-humanize"
	"github.com/vmihailenco/msgpack/v5"
)

func List(db *badger.DB) error {
	return db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 1000
		it := txn.NewIterator(opts)
		defer it.Close()

		eviPrefix := []byte(cnst.EviFileNamespace)
		for it.Seek(eviPrefix); it.ValidForPrefix(eviPrefix); it.Next() {
			item := it.Item()
			k := item.KeyCopy(nil)
			v, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}

			decoded, err := cnst.DECODER.DecodeAll(v, nil)
			if err == nil {
				v = decoded
			}

			var evidata structs.EvidenceFile
			err = msgpack.Unmarshal(v, &evidata)
			if err != nil {
				return err
			}

			if !evidata.Completed {
				continue
			}

			evihash := bytes.Split(k, eviPrefix)[1]
			fmt.Println(base64.StdEncoding.EncodeToString(evihash))
			fmt.Printf("\tNames: %v\n", evidata.Names)
			fmt.Printf("\tSize: %v\n", humanize.Bytes(uint64(evidata.Size)))
			for _, phash := range evidata.InternalObjects {
				err = listPartitions(phash, txn)
				if err != nil {
					return err
				}
			}
		}

		return nil
	})
}

func listPartitions(phash string, txn *badger.Txn) error {
	phash = strings.Split(phash, cnst.DataSeperator)[0]
	fmt.Printf("\tPartition: %v\n", phash)
	decodedPhash, err := base64.StdEncoding.DecodeString(phash)
	if err != nil {
		return err
	}

	pid := append([]byte(cnst.PartiFileNamespace), decodedPhash...)
	item, err := txn.Get(pid)
	if err != nil {
		return err
	}
	v, err := item.ValueCopy(nil)
	if err != nil {
		return err
	}

	decoded, err := cnst.DECODER.DecodeAll(v, nil)
	if err == nil {
		v = decoded
	}

	var pdata structs.PartitionFile
	err = msgpack.Unmarshal(v, &pdata)
	if err != nil {
		return err
	}

	for i, ihash := range pdata.InternalObjects {
		err = listIndexedFiles(i, ihash, txn)
		if err != nil {
			return err
		}
	}

	return nil
}

func listIndexedFiles(index int, ihash string, txn *badger.Txn) error {
	ihash = strings.Split(ihash, cnst.DataSeperator)[0]
	fmt.Printf("\t\tIndexed %d ---> %s\n", index, ihash)
	decidedIhash, err := base64.StdEncoding.DecodeString(ihash)
	if err != nil {
		return err
	}

	pid := append([]byte(cnst.IdxFileNamespace), decidedIhash...)
	item, err := txn.Get(pid)
	if err != nil {
		return err
	}
	v, err := item.ValueCopy(nil)
	if err != nil {
		return err
	}

	decoded, err := cnst.DECODER.DecodeAll(v, nil)
	if err == nil {
		v = decoded
	}

	var idata structs.IndexedFile
	err = msgpack.Unmarshal(v, &idata)
	if err != nil {
		return err
	}

	fmt.Printf("\t\tNames: %v", idata.Names)

	return nil
}
