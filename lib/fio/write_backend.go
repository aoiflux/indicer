package fio

import (
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"io"
	"os"
	"sync"
)

type writeBackend interface {
	Name() string
	WriteBlob(path string, data []byte) error
	WriteToFile(file *os.File, data []byte) (int, error)
}

type stdlibWriteBackend struct{}

func (stdlibWriteBackend) Name() string {
	return cnst.StoreIOEngineStdlib
}

func (stdlibWriteBackend) WriteBlob(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, cnst.FilePerm)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}

	if _, err := writeAll(f, data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}

	return nil
}

func (stdlibWriteBackend) WriteToFile(file *os.File, data []byte) (int, error) {
	return writeAll(file, data)
}

func writeAll(file *os.File, data []byte) (int, error) {
	total := 0
	for total < len(data) {
		n, err := file.Write(data[total:])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

type ioUringWriteBackend struct {
	delegate stdlibWriteBackend
	handleMu sync.Mutex
	handle   ioUringHandle
	initErr  error
	initOnce sync.Once
}

type ioUringHandle interface {
	Close() error
	SubmitWrite(fd int, data []byte) (int, error)
}

func (*ioUringWriteBackend) Name() string {
	return cnst.StoreIOEngineUring
}

func (b *ioUringWriteBackend) ensureHandle() (ioUringHandle, error) {
	b.initOnce.Do(func() {
		h, err := openIoUringHandle()
		if err != nil {
			b.initErr = err
			return
		}
		b.handleMu.Lock()
		b.handle = h
		b.handleMu.Unlock()
	})

	if b.initErr != nil {
		return nil, b.initErr
	}

	b.handleMu.Lock()
	h := b.handle
	b.handleMu.Unlock()
	if h == nil {
		return nil, errors.New("io-uring handle unavailable")
	}
	return h, nil
}

var (
	backendOnce        sync.Once
	selectedBackend    writeBackend = stdlibWriteBackend{}
	backendWarningOnce sync.Once
)

func getWriteBackend() writeBackend {
	backendOnce.Do(initWriteBackend)
	return selectedBackend
}

func initWriteBackend() {
	engine := cnst.NormalizeStoreIOEngine(cnst.StoreIOEngine)
	if engine == "" {
		engine = cnst.StoreIOEngineAuto
	}

	switch engine {
	case cnst.StoreIOEngineUring:
		if err := probeIoUringSupport(); err != nil {
			warnBackendFallback(engine, err.Error())
			selectedBackend = stdlibWriteBackend{}
			return
		}
		selectedBackend = &ioUringWriteBackend{}
	case cnst.StoreIOEngineStdlib, cnst.StoreIOEngineAuto:
		selectedBackend = stdlibWriteBackend{}
	default:
		selectedBackend = stdlibWriteBackend{}
	}
}

func warnBackendFallback(requested, reason string) {
	backendWarningOnce.Do(func() {
		_, _ = fmt.Fprintf(os.Stderr, "[fio] store io engine %q fallback to %q: %s\n", requested, cnst.StoreIOEngineStdlib, reason)
	})
}
