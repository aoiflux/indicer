package fio

import (
	"encoding/base64"
	"errors"
	"indicer/lib/cnst"
	"indicer/lib/fsx"
	"indicer/lib/util"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteChonk stores data, a chunk's encoded bytes, as the blob for the chunk
// key ckey in the blob directory beside the database at dbpath, and returns
// the blob's path.
//
// The blob is synced before WriteChonk returns (STO-4). Ingest writes the
// chunk's C||| metadata only after that, and nothing trusts a blob without
// metadata recording its size, so a blob torn by a crash is never read.
//
// A new blob is created in place, which avoids a rename per chunk. An
// existing blob is replaced through a temporary file and an atomic rename:
// ingest calls this only for a chunk whose blob is missing or not the size
// its metadata records, so an existing file is a torn or truncated blob, and
// the metadata that may still point at it must never see a short file.
func WriteChonk(dbpath string, data, ckey, key []byte) ([]byte, error) {
	ckhash, err := util.GetChonkHash(ckey, cnst.GetHashAlgo())
	if err != nil {
		return nil, err
	}
	cfname := base64.RawURLEncoding.EncodeToString(ckhash) + cnst.BLOBEXT
	cfpath := filepath.Join(util.BlobPath(dbpath), cfname)
	err = fsx.WriteNewFile(cfpath, data, cnst.FilePerm)
	if errors.Is(err, fs.ErrExist) {
		err = fsx.WriteFileAtomic(cfpath, data, cnst.FilePerm)
	}
	if err != nil {
		return nil, err
	}
	return []byte(cfpath), nil
}

// ReadChonk returns the stored (still encoded) bytes of a chunk blob;
// lib/blobcodec decodes them.
func ReadChonk(cfpath string) ([]byte, error) {
	return os.ReadFile(cfpath)
}
