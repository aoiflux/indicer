package server

import (
	"context"
	"encoding/base64"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/service"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
	"indicer/pb"
	"strings"

	"github.com/dgraph-io/badger/v4"
)

func (g *GrpcService) GetIdxFiles(ctx context.Context, req *pb.GetIdxFilesReq) (*pb.GetIdxFilesRes, error) {
	if req.PartiFileId == "" {
		return nil, cnst.ErrHashNotFound
	}
	partiFileId, err := base64.StdEncoding.DecodeString(req.PartiFileId)
	if err != nil {
		return nil, err
	}

	partiFile, err := dbio.GetPartitionFile(partiFileId, cnst.DB)
	if err != nil {
		return nil, err
	}

	baseFiles, err := getIdxFiles(partiFile.InternalObjects, cnst.DB)
	if err != nil {
		return nil, err
	}

	var res pb.GetIdxFilesRes
	res.Done = true
	res.Err = ""
	res.IndexedFile = baseFiles
	return &res, nil
}

func getIdxFiles(idxMap map[string]structs.InternalOffset, db *badger.DB) ([]*pb.BaseFile, error) {
	var baseList []*pb.BaseFile

	for ihash := range idxMap {
		decodedPhash, err := base64.StdEncoding.DecodeString(ihash)
		if err != nil {
			return nil, err
		}

		iid := util.AppendToBytesSlice(cnst.IdxFileNamespace, decodedPhash)
		idxfile, err := dbio.GetIndexedFile(iid, db)
		if err != nil {
			return nil, err
		}

		ehash, err := store.GetLogicalFileEviHash(idxfile.Names, db)
		if err != nil {
			return nil, err
		}

		chunkMap, err := service.GetFileChunkMap(idxfile.Size, ehash)
		if err != nil {
			return nil, err
		}

		for name := range idxfile.Names {
			var baseFile pb.BaseFile
			baseFile.FileId = base64.StdEncoding.EncodeToString(iid)

			split := strings.Split(name, cnst.DataSeperator)
			name = split[len(split)-1]

			baseFile.FilePath = name
			baseFile.FileSize = idxfile.Size
			baseFile.ChunkMap = chunkMap
			baseList = append(baseList, &baseFile)
		}
	}

	return baseList, nil
}
