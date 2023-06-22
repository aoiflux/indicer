package structs

import (
	"github.com/dgraph-io/badger/v3"
	"github.com/edsrzf/mmap-go"
)

type ThreadIO struct {
	Index      int64
	ChonkEnd   int64
	MappedFile mmap.MMap
	FHash      []byte
	DB         *badger.DB
	Batch      *badger.WriteBatch
	Err        chan error
}
