package server

import (
	"context"
	"encoding/base64"
	"indicer/lib/cnst"
	"indicer/lib/util"
	"indicer/pb"
	"indicer/service"
)

func (g *GrpcService) AppendIfExists(ctx context.Context, req *pb.AppendIfExistsReq) (*pb.AppendIfExistsRes, error) {
	var res pb.AppendIfExistsRes

	efile, existence, chkApndErr := service.CheckAndAppend(req.FilePath, req.FileHash, cnst.DB)
	if chkApndErr != nil {
		if chkApndErr == cnst.ErrFileNotFound {
			res.Exists = false
			return &res, nil
		}
		return nil, chkApndErr
	}

	chunkMap, chunkErr := service.GetEviFileChunkMap(efile.Size, req.FileHash)
	if chunkErr != nil {
		return nil, chunkErr
	}
	res.EviFile.FilePath = req.FilePath
	res.EviFile.ChunkMap = chunkMap

	fileHash, err := base64.StdEncoding.DecodeString(req.FileHash)
	if err != nil {
		return nil, err
	}
	eid := util.AppendToBytesSlice(cnst.EviFileNamespace, fileHash)
	fileId := base64.StdEncoding.EncodeToString(eid)
	res.EviFile.FileId = fileId

	if existence == cnst.FILE_APPENDED {
		res.Appended = true
	}
	res.Exists = true
	return &res, nil
}
