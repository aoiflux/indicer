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

func (g *GrpcService) GetPartiFiles(ctx context.Context, req *pb.GetPartiFilesReq) (*pb.GetPartiFilesRes, error) {
	if req.EviFileId == "" {
		return nil, cnst.ErrHashNotFound
	}
	eviFileId, err := base64.StdEncoding.DecodeString(req.EviFileId)
	if err != nil {
		return nil, err
	}

	eviFile, err := dbio.GetEvidenceFile(eviFileId, cnst.DB)
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

		ehash, err := store.GetLogicalFileEviHash(partiFile.Names, db)
		if err != nil {
			return nil, err
		}

		chunkMap, err := service.GetFileChunkMap(partiFile.Start, partiFile.Size, ehash)
		if err != nil {
			return nil, err
		}

		for name := range partiFile.Names {
			var baseFile pb.BaseFile
			baseFile.FileId = base64.StdEncoding.EncodeToString(pid)

			split := strings.Split(name, cnst.DataSeperator)
			name = split[len(split)-1]

			baseFile.FilePath = name
			baseFile.FileSize = partiFile.Size
			baseFile.ChunkMap = chunkMap
			baseList = append(baseList, &baseFile)
		}
	}

	return baseList, nil
}
