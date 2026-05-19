package fio

import (
	"encoding/base64"
	"indicer/lib/cnst"
	"indicer/lib/util"
	"os"
	"path/filepath"
)

func WriteChonk(dbpath string, data, ckey, key []byte) ([]byte, error) {
	var err error
	if !cnst.QUICKOPT {
		data = cnst.ENCODER.EncodeAll(data, make([]byte, 0, len(data)))
		data, err = util.SealAES(key, data)
		if err != nil {
			return nil, err
		}
	}

	ckhash, err := util.GetChonkHash(ckey, cnst.GetHashAlgo())
	if err != nil {
		return nil, err
	}
	cfname := base64.RawURLEncoding.EncodeToString(ckhash) + cnst.BLOBEXT
	cfpath := filepath.Join(util.BlobPath(dbpath), cfname)
	err = os.WriteFile(cfpath, data, cnst.FilePerm)
	return []byte(cfpath), err
}

func ReadChonk(cfpath, key []byte) ([]byte, error) {
	encoded, err := os.ReadFile(string(cfpath))
	if err != nil {
		return nil, err
	}
	if cnst.QUICKOPT {
		return encoded, nil
	}

	decrypted, err := util.UnsealAES(key, encoded)
	if err != nil {
		return nil, err
	}
	decoded, err := cnst.DECODER.DecodeAll(decrypted, nil)
	if err != nil {
		return nil, err
	}

	return decoded, nil
}
