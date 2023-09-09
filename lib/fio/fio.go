package fio

import (
	"indicer/lib/cnst"
	"indicer/lib/util"
	"os"
	"path/filepath"
)

func WriteChonk(dbpath string, data, key []byte) ([]byte, error) {
	var err error
	if !cnst.QUICKOPT {
		data = cnst.ENCODER.EncodeAll(data, make([]byte, 0, len(data)))
		data, err = util.SealAES(key, data)
		if err != nil {
			return nil, err
		}
	}
	cfname := util.GetRandomName(cnst.FileNameLen) + cnst.BLOBEXT
	cfpath := filepath.Join(dbpath, cnst.BLOBSDIR, cfname)
	err = os.WriteFile(cfpath, data, os.ModePerm)
	return []byte(cfpath), err
}

func ReadChonk(cfpath, key []byte) ([]byte, error) {
	var data []byte

	encoded, err := os.ReadFile(string(cfpath))
	if err != nil {
		return nil, err
	}
	decrypted, err := util.UnsealAES(key, encoded)
	if err == nil {
		data = decrypted
	}
	decoded, err := cnst.DECODER.DecodeAll(data, nil)
	if err == nil {
		data = decoded
	}

	return data, nil
}
