package fio

import (
	"encoding/base64"
	"errors"
	"indicer/lib/cnst"
	"indicer/lib/util"
	"os"
	"path/filepath"
)

func WriteChonk(dbpath string, data, ckey, key []byte) ([]byte, error) {
	var err error

	ckhash, err := util.GetChonkHash(ckey, cnst.GetHashAlgo())
	if err != nil {
		return nil, err
	}
	cfname := base64.RawURLEncoding.EncodeToString(ckhash) + cnst.BLOBEXT
	cfpath := filepath.Join(util.BlobPath(dbpath), cfname)

	if cnst.StoreAsyncFileWrites {
		if err := EnqueueAsyncFileWrite(cfpath, data); err != nil {
			return nil, err
		}
		return []byte(cfpath), nil
	}

	f, err := os.OpenFile(cfpath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, cnst.FilePerm)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			// Idempotent content-addressed write: an existing blob is expected.
			return []byte(cfpath), nil
		}
		return nil, err
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(cfpath)
		return nil, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(cfpath)
		return nil, err
	}

	return []byte(cfpath), nil
}

// ReadChonk returns the stored (still encoded) bytes of a chunk blob;
// lib/blobcodec decodes them.
func ReadChonk(cfpath string) ([]byte, error) {
	return os.ReadFile(cfpath)
}
