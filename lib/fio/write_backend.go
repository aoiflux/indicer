package fio

import (
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"io"
	"os"
	"sync"
	"sync/atomic"
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
	backendFallbacks   uint64

	ioUringSubmitAttempts uint64
	ioUringSubmitWaits    uint64
	ioUringQueueFullHits  uint64
	ioUringSubmitErrors   uint64
	ioUringCompletions    uint64
)

type WriteBackendStats struct {
	RequestedEngine     string
	SelectedEngine      string
	FallbackCount       uint64
	IOUringQueueDepth   int
	IOUringSubmitCount  uint64
	IOUringSubmitWaits  uint64
	IOUringQueueFull    uint64
	IOUringSubmitErrors uint64
	IOUringCompletions  uint64
}

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
	atomic.AddUint64(&backendFallbacks, 1)
	backendWarningOnce.Do(func() {
		_, _ = fmt.Fprintf(os.Stderr, "[fio] store io engine %q fallback to %q: %s\n", requested, cnst.StoreIOEngineStdlib, reason)
	})
}

func recordIoUringSubmitAttempt() {
	atomic.AddUint64(&ioUringSubmitAttempts, 1)
}

func recordIoUringSubmitWait() {
	atomic.AddUint64(&ioUringSubmitWaits, 1)
}

func recordIoUringQueueFull() {
	atomic.AddUint64(&ioUringQueueFullHits, 1)
}

func recordIoUringSubmitError() {
	atomic.AddUint64(&ioUringSubmitErrors, 1)
}

func recordIoUringCompletion() {
	atomic.AddUint64(&ioUringCompletions, 1)
}

func GetWriteBackendStats() WriteBackendStats {
	requested := cnst.NormalizeStoreIOEngine(cnst.StoreIOEngine)
	if requested == "" {
		requested = cnst.StoreIOEngineAuto
	}

	selected := cnst.StoreIOEngineStdlib
	if selectedBackend != nil {
		selected = selectedBackend.Name()
	}

	depth := 0
	if requested == cnst.StoreIOEngineUring || selected == cnst.StoreIOEngineUring {
		depth = cnst.GetStoreIOUringQueueDepth()
	}

	return WriteBackendStats{
		RequestedEngine:     requested,
		SelectedEngine:      selected,
		FallbackCount:       atomic.LoadUint64(&backendFallbacks),
		IOUringQueueDepth:   depth,
		IOUringSubmitCount:  atomic.LoadUint64(&ioUringSubmitAttempts),
		IOUringSubmitWaits:  atomic.LoadUint64(&ioUringSubmitWaits),
		IOUringQueueFull:    atomic.LoadUint64(&ioUringQueueFullHits),
		IOUringSubmitErrors: atomic.LoadUint64(&ioUringSubmitErrors),
		IOUringCompletions:  atomic.LoadUint64(&ioUringCompletions),
	}
}
