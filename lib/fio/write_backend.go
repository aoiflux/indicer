package fio

import (
	"fmt"
	"indicer/internal/console"
	"indicer/lib/cnst"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// writeBackend writes container frames. Writes are positioned, so a retried
// or repeated write of the same bytes at the same offset changes nothing.
type writeBackend interface {
	Name() string
	WriteAt(file *os.File, data []byte, off int64) (int, error)
}

type stdlibWriteBackend struct{}

func (stdlibWriteBackend) Name() string {
	return cnst.StoreIOEngineStdlib
}

func (stdlibWriteBackend) WriteAt(file *os.File, data []byte, off int64) (int, error) {
	total := 0
	for total < len(data) {
		n, err := file.WriteAt(data[total:], off+int64(total))
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

func (*ioUringWriteBackend) Name() string {
	return cnst.StoreIOEngineUring
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
		_, _ = fmt.Fprintf(console.Err(), "[fio] store io engine %q fallback to %q: %s\n", requested, cnst.StoreIOEngineStdlib, reason)
	})
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
