package service

import (
	"encoding/base64"
	"indicer/cli"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"indicer/pb"
	"strings"
)

func StoreStreamedFile(fpath string) error {
	key := util.HashPassword("")
	return cli.StoreFile(int(cnst.DefaultChonkSize), fpath, key, false, false, false, false, cnst.DB)
}

func AddEvidenceMetadata(meta *pb.StreamFileMeta) (structs.EvidenceFile, error) {
	efile, err := getEvidenceFile(meta.FilePath, meta.FileHash, cnst.DB)
	if err != nil {
		return efile, err
	}
	fileType := strings.ToLower(strings.TrimSpace(meta.FileType))
	if fileType != "" && fileType != cnst.UnknownEvidenceType {
		efile.EvidenceType = fileType
	}

	fileHash, err := base64.StdEncoding.DecodeString(meta.FileHash)
	if err != nil {
		return efile, err
	}
	eid := util.AppendToBytesSlice(cnst.EviFileNamespace, fileHash)

	err = dbio.SetFile(eid, efile, cnst.DB)
	return efile, err
}
