package server

import (
	"context"
	"encoding/base64"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/service"
	"indicer/lib/structs"
	"indicer/lib/util"
	"indicer/pb"

	"github.com/dgraph-io/badger/v4"
)

func (g *GrpcService) GetPartiFiles(ctx context.Context, req *pb.GetPartiFilesReq) (*pb.GetPartiFilesRes, error) {
	if req.EviFileId == "" {
		return nil, cnst.ErrHashNotFound
	}

	eviFile, err := dbio.GetEvidenceFile([]byte(req.EviFileId), cnst.DB)
	if err != nil {
		return nil, err
	}
	baseFiles, err := getPartiFiles(eviFile.InternalObjects, cnst.DB)
	if err != nil {
		return nil, err
	}

	var res pb.GetPartiFilesRes
	res.Done = true
	res.Err = ""
	res.PartitionFile = baseFiles
	return &res, nil
}

func getPartiFiles(partiMap map[string]structs.InternalOffset, db *badger.DB) ([]*pb.BaseFile, error) {
	var baseList []*pb.BaseFile

	for phash := range partiMap {
		decodedPhash, err := base64.StdEncoding.DecodeString(phash)
		if err != nil {
			return nil, err
		}

		pid := util.AppendToBytesSlice(cnst.PartiFileNamespace, decodedPhash)
		partiFile, err := dbio.GetPartitionFile(pid, db)
		if err != nil {
			return nil, err
		}

		chunkMap, err := service.GetFileChunkMap(partiFile.Size, phash)
		if err != nil {
			return nil, err
		}

		for name := range partiFile.Names {
			var baseFile pb.BaseFile
			baseFile.FileId = base64.StdEncoding.EncodeToString(pid)
			baseFile.FilePath = name
			baseFile.FileSize = partiFile.Size
			baseFile.ChunkMap = chunkMap
			baseList = append(baseList, &baseFile)
		}
	}

	return baseList, nil
}
