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

	"github.com/dgraph-io/badger/v4"
)

func StoreStreamedFile(fpath string) error {
	key, err := util.HashPassword("")
	if err != nil {
		return err
	}
	return cli.StoreFile(int(cnst.DefaultChonkSize), fpath, key, false, false, false, false, cnst.DB)
}

func AddEvidenceMetadata(meta *pb.StreamFileMeta) (structs.EvidenceFile, error) {
	efile, err := getEvidenceFile(meta.FilePath, meta.FileHash, cnst.DB)
	if err != nil {
		return efile, err
	}
	fileType := strings.ToLower(strings.TrimSpace(meta.FileType))
	if fileType == "" || fileType == cnst.UnknownEvidenceType {
		return efile, nil
	}

	fileHash, err := base64.StdEncoding.DecodeString(meta.FileHash)
	if err != nil {
		return efile, err
	}
	eid := util.AppendToBytesSlice(cnst.EviFileNamespace, fileHash)

	err = dbio.UpdateEvidence(cnst.DB, eid, func(_ *badger.Txn, current *structs.EvidenceFile) error {
		current.EvidenceType = fileType
		efile = *current
		return nil
	})
	return efile, err
}
