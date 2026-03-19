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
		opts.PrefetchSize = 1000
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

			var evidata structs.EvidenceFile
			err = msgpack.Unmarshal(v, &evidata)
			if err != nil {
				return err
			}

			if !evidata.Completed {
				continue
			}

			evihash := bytes.Split(id, eviPrefix)[1]
			eviHashStr := base64.StdEncoding.EncodeToString(evihash)
			chunkMap, err := service.GetFileChunkMap(evidata.Size, eviHashStr)
			if err != nil {
				return err
			}

			idstr := base64.StdEncoding.EncodeToString(id)
			for name := range evidata.Names {
				var eviFile pb.BaseFile

				eviFile.FileId = idstr
				eviFile.FilePath = name
				eviFile.FileSize = evidata.Size
				eviFile.ChunkMap = chunkMap

				eviList = append(eviList, &eviFile)
			}
		}

		return nil
	})

	return eviList, err
}
