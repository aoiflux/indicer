//go:build !linux

package fio

import (
	"errors"
	"os"
)

func openIoUringHandle() (ioUringHandle, error) {
	return nil, errors.New("io-uring is only available on linux")
}

func (b *ioUringWriteBackend) WriteAt(file *os.File, data []byte, off int64) (int, error) {
	return b.delegate.WriteAt(file, data, off)
}
