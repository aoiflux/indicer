package store

import (
	"encoding/base64"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/structs"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/klauspost/compress/s2"
	"github.com/vmihailenco/msgpack/v5"
	"go.etcd.io/bbolt"
)

func List(db *bbolt.DB) error {
	return db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(cnst.EviBucket))
		return bucket.ForEach(func(k, v []byte) error {
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
				return nil
			}

			fmt.Println(base64.StdEncoding.EncodeToString(k))
			fmt.Printf("\tNames: %v\n", evidata.Names)
			fmt.Printf("\tSize: %v\n", humanize.Bytes(uint64(evidata.Size)))
			for _, phash := range evidata.InternalObjects {
				err = listPartitions(phash, tx)
				if err != nil {
					return err
				}
			}

			return nil
		})
	})
}

func listPartitions(phash string, txn *bbolt.Tx) error {
	phash = strings.Split(phash, cnst.DataSeperator)[0]
	fmt.Printf("\tPartition: %v\n", phash)
	decodedPhash, err := base64.StdEncoding.DecodeString(phash)
	if err != nil {
		return err
	}

	bucket := txn.Bucket([]byte(cnst.PartiBucket))
	item := bucket.Get(decodedPhash)
	if item == nil {
		return cnst.ErrKeyNotFound
	}
	decoded, err := s2.Decode(nil, item)
	if err != nil {
		return err
	}
	var pdata structs.PartitionFile
	err = msgpack.Unmarshal(decoded, &pdata)
	if err != nil {
		return err
	}

	for i, ihash := range pdata.InternalObjects {
		ihash = strings.Split(ihash, cnst.DataSeperator)[0]
		fmt.Printf("\t\tIndexed %d ---> %s\n", i, ihash)
	}

	return nil
}
