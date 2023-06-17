package structs

import (
	"os"

	"github.com/dgraph-io/badger/v3"
)

type ThreadIO struct {
	Index    int64
	BuffSize int64
	FHandle  *os.File
	FHash    []byte
	DB       *badger.DB
	Batch    *badger.WriteBatch
	Err      chan error
}
