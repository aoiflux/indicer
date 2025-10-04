package service

import (
	"encoding/base64"
	"indicer/cli"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"indicer/pb"
	"path/filepath"
)

func StoreStreamedFile(fname string) error {
	fpath := filepath.Join(cnst.UploadsDir, fname)
	fpath, err := filepath.Abs(fpath)
	if err != nil {
		return err
	}

	key := util.HashPassword("")
	return cli.StoreFile(int(cnst.DefaultChonkSize), fpath, key, false, false, cnst.DB)
}

func AddEvidenceMetadata(meta *pb.StreamFileMeta) (structs.EvidenceFile, error) {
	efile, err := getEvidenceFile(meta.FilePath, meta.FileHash, cnst.DB)
	if err != nil {
		return efile, err
	}
	efile.EvidenceType = meta.FileType

	eid := util.AppendToBytesSlice(cnst.EviFileNamespace, meta.FileHash)
	err = dbio.SetFile(eid, efile, cnst.DB)
	return efile, err
}

func GetEviFileChunkMap(fileSize int64, fileHashStr string) (map[string]int64, error) {
	fileHash, err := base64.StdEncoding.DecodeString(fileHashStr)
	if err != nil {
		return nil, err
	}

	var meta structs.FileMeta
	meta.EviHash = fileHash
	meta.Size = fileSize
	meta.Start = 0

	return getChunkMap(meta, cnst.DB)
}
