package server

import (
	"context"
	"encoding/base64"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/pb"
)

func (g *GrpcService) GetIdxFiles(ctx context.Context, req *pb.GetIdxFilesReq) (*pb.GetIdxFilesRes, error) {
	if req.PartiFileId == "" {
		return nil, cnst.ErrHashNotFound
	}
	partiFileId, err := base64.StdEncoding.DecodeString(req.PartiFileId)
	if err != nil {
		return nil, err
	}

	_, err = dbio.GetPartitionFile(partiFileId, cnst.DB)
	if err != nil {
		return nil, err
	}

	var res pb.GetIdxFilesRes
	// todo - implement this function
	return &res, nil
}
