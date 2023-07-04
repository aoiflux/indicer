package structs

import (
	"github.com/edsrzf/mmap-go"
	"go.etcd.io/bbolt"
)

type ThreadIO struct {
	Index      int64
	ChonkEnd   int64
	MappedFile mmap.MMap
	FHash      []byte
	DB         *bbolt.DB
	Err        chan error
}
