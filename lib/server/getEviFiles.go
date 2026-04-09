package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"indicer/lib/cnst"
	"indicer/lib/service"
	"indicer/lib/structs"
	"indicer/pb"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

func (g *GrpcService) GetEviFiles(ctx context.Context, req *pb.GetEviFilesReq) (*pb.GetEviFilesRes, error) {
	eviList, err := getBaseFiles(cnst.EviFileNamespace, cnst.DB)
	if err != nil {
		return nil, err
	}

	var res pb.GetEviFilesRes
	res.Done = true
	res.Err = ""
	res.EviFile = eviList
	return &res, nil
}

func getBaseFiles(prefix string, db *badger.DB) ([]*pb.BaseFile, error) {
	var eviList []*pb.BaseFile

	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		it := txn.NewIterator(opts)
		defer it.Close()

		eviPrefix := []byte(prefix)
		for it.Seek(eviPrefix); it.ValidForPrefix(eviPrefix); it.Next() {
			item := it.Item()
			id := item.KeyCopy(nil)
			v, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}

			decoded, err := cnst.DECODER.DecodeAll(v, nil)
			if err == nil {
				v = decoded
			}

			var eviFile structs.EvidenceFile
			err = msgpack.Unmarshal(v, &eviFile)
			if err != nil {
				return err
			}

			if !eviFile.Completed {
				continue
			}

			ehash := bytes.Split(id, eviPrefix)[1]
			chunkMap, err := service.GetFileChunkMap(eviFile.Start, eviFile.Size, ehash)
			if err != nil {
				return err
			}

			idstr := base64.StdEncoding.EncodeToString(id)
			for name := range eviFile.Names {
				var baseFile pb.BaseFile

				baseFile.FileId = idstr
				baseFile.FilePath = name
				baseFile.FileSize = eviFile.Size
				baseFile.ChunkMap = chunkMap

				eviList = append(eviList, &baseFile)
			}
		}

		return nil
	})

	return eviList, err
}
