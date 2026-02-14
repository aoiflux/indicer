package structs

import (
	"indicer/lib/fio"

	"github.com/dgraph-io/badger/v4"
	"github.com/edsrzf/mmap-go"
)

type ThreadIO struct {
	Index        int64
	ChonkEnd     int64
	MappedFile   mmap.MMap
	FHash        []byte
	DB           *badger.DB
	Batch        *badger.WriteBatch
	Err          chan error
	ContainerMgr *fio.ContainerManager
}
