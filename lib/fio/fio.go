package fio

import (
	"indicer/lib/cnst"
	"indicer/lib/util"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/s2"
)

func WriteChonk(dbpath string, chonk, key []byte) ([]byte, error) {
	encoded := s2.EncodeBest(nil, chonk)
	cfname := util.GetRandomName(cnst.FileNameLen) + cnst.BLOBEXT
	cfpath := filepath.Join(dbpath, cnst.BLOBSDIR, cfname)
	err := os.WriteFile(cfpath, encoded, os.ModePerm)
	return []byte(cfpath), err
}

func ReadChonk(cfpath, key []byte) ([]byte, error) {
	encoded, err := os.ReadFile(string(cfpath))
	if err != nil {
		return nil, err
	}
	return s2.Decode(nil, encoded)
}
