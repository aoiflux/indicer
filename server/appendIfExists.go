package server

import (
	"context"
	"indicer/lib/cnst"
	"indicer/pb"
	"indicer/service"
)

func (g *GrpcService) AppendIfExists(ctx context.Context, req *pb.AppendIfExistsReq) (*pb.AppendIfExistsRes, error) {
	var res pb.AppendIfExistsRes

	efile, existence, chkApndErr := service.CheckAndAppend(req.FilePath, req.FileHash, cnst.DB)

	chunkMap, chunkErr := service.GetEviFileChunkMap(efile.Size, req.FileHash)
	if chunkErr != nil {
		return nil, chunkErr
	}
	res.EviFile.FilePath = req.FilePath
	res.EviFile.ChunkMap = chunkMap

	if chkApndErr != nil {
		if chkApndErr == cnst.ErrFileNotFound {
			res.Appended = false
			res.Exists = false
			res.Err = ""

			return &res, nil
		}

		return nil, chkApndErr
	}

	if existence == cnst.FILE_APPENDED {
		res.Appended = true
	}
	res.Exists = true
	return &res, nil
}
