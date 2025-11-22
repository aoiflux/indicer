package server

import (
	"encoding/base64"
	"errors"
	"indicer/lib/cnst"
	"indicer/lib/util"
	"indicer/pb"
	"indicer/service"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"google.golang.org/grpc"
)

func (g *GrpcService) StreamFile(stream grpc.ClientStreamingServer[pb.StreamFileReq, pb.StreamFileRes]) error {
	req, err := stream.Recv()
	if err != nil {
		return err
	}

	meta := req.GetFileMeta()
	if meta == nil {
		log.Println("Expected metadata with file")
		return errors.New("metadata not found - please try again")
	}

	fname, err := uploadFile(stream)
	if err != nil {
		return err
	}
	err = service.StoreStreamedFile(fname)
	if err != nil {
		return err
	}
	efile, err := service.AddEvidenceMetadata(meta)
	if err != nil {
		return err
	}
	chunkMap, err := service.GetEviFileChunkMap(efile.Size, meta.FileHash)
	if err != nil {
		return err
	}

	var res pb.StreamFileRes
	res.Done = true
	res.Err = ""
	res.EviFile.FilePath = meta.FilePath
	res.EviFile.ChunkMap = chunkMap

	eid := util.AppendToBytesSlice(cnst.EviFileNamespace, meta.FileHash)
	fileId := base64.StdEncoding.EncodeToString(eid)
	res.EviFile.FileId = fileId

	return stream.SendAndClose(&res)
}

func uploadFile(stream grpc.ClientStreamingServer[pb.StreamFileReq, pb.StreamFileRes]) (string, error) {
	fh, err := getFileHandle()
	if err != nil {
		return "", err
	}

	for {
		req, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", err
		}

		chunk := req.GetFile()
		_, err = fh.Write(chunk)
		if err != nil {
			return "", err
		}
	}

	fname := fh.Name()
	err = fh.Close()
	if err != nil {
		return "", nil
	}

	log.Printf("Uploaded file to ")
	return fname, nil
}
func getFileHandle() (*os.File, error) {
	fid, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}

	fpath := filepath.Join(cnst.UploadsDir, fid.String())
	fpath, err = filepath.Abs(fpath)
	if err != nil {
		return nil, err
	}

	return os.Create(fpath)
}
