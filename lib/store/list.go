package store

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v3"
	"github.com/dustin/go-humanize"
	"github.com/klauspost/compress/s2"
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

			decoded, err := s2.Decode(nil, v)
			if err != nil {
				return err
			}

			var evidata structs.EvidenceFile
			err = msgpack.Unmarshal(decoded, &evidata)
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

func listPartitions(phash []byte, txn *badger.Txn) error {
	phash = bytes.Split(phash, []byte(cnst.DataSeperator))[0]
	fmt.Println("Partition: ", base64.StdEncoding.EncodeToString(phash))
	pid := append([]byte(cnst.PartiFileNamespace), phash...)
	item, err := txn.Get(pid)
	if err != nil {
		return err
	}
	v, err := item.ValueCopy(nil)
	if err != nil {
		return err
	}
	decoded, err := s2.Decode(nil, v)
	if err != nil {
		return err
	}
	var pdata structs.PartitionFile
	err = msgpack.Unmarshal(decoded, &pdata)
	if err != nil {
		return err
	}

	for i, ihash := range pdata.InternalObjects {
		ihash = bytes.Split(ihash, []byte(cnst.DataSeperator))[0]
		ihashStr := base64.StdEncoding.EncodeToString(ihash)
		fmt.Printf("\tIndexed %d ---> %s\n", i, ihashStr)
	}

	return nil
}
