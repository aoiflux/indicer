package structs

import (
	"os"

	"github.com/dgraph-io/badger/v4"
)

type ThreadIO struct {
	Index      int64
	ChonkEnd   int64
	ChonkSize  int64
	FileHandle *os.File
	FHash      []byte
	DB         *badger.DB
	Batch      *badger.WriteBatch
	Err        chan error
}
