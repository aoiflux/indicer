package server

import (
	"context"
	"indicer/lib/cnst"
	"indicer/pb"
	"indicer/service"
)

func (g *GrpcService) AppendIfExists(ctx context.Context, req *pb.AppendIfExistsReq) (*pb.AppendIfExistsRes, error) {
	var res pb.AppendIfExistsRes

	existence, err := service.CheckAndAppend(req.FilePath, req.FileHash, cnst.DB)
	if err != nil {
		if err == cnst.ErrFileNotFound {
			res.Appended = false
			res.Exists = false
			res.Err = ""

			return &res, nil
		}

		return nil, err
	}

	if existence == cnst.FILE_APPENDED {
		res.Appended = true
	}
	res.Exists = true
	return &res, nil
}
