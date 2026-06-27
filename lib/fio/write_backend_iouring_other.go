//go:build !linux

package fio

import (
	"errors"
	"os"
)

func openIoUringHandle() (ioUringHandle, error) {
	return nil, errors.New("io-uring is only available on linux")
}

func (b *ioUringWriteBackend) WriteBlob(path string, data []byte) error {
	return b.delegate.WriteBlob(path, data)
}

func (b *ioUringWriteBackend) WriteToFile(file *os.File, data []byte) (int, error) {
	return b.delegate.WriteToFile(file, data)
}
