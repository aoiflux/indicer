//go:build !linux

package fio

import "os"

// ioUringWriteBackend writes through the standard library where io-uring
// does not exist; the engine is never selected there (probeIoUringSupport).
type ioUringWriteBackend struct {
	delegate stdlibWriteBackend
}

func (b *ioUringWriteBackend) WriteAt(file *os.File, data []byte, off int64) (int, error) {
	return b.delegate.WriteAt(file, data, off)
}
