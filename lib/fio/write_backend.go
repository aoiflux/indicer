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
	// writeEngine and uringDepth are the process's store io engine and
	// io-uring queue depth (SetWriteEngine); the backend is chosen from them
	// on the first container write.
	writeEngine = cnst.StoreIOEngineAuto
	uringDepth  int

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

// SetWriteEngine sets the store io engine (cnst.StoreIOEngine*) and the
// io-uring queue depth (0 = auto) container files are written with. The
// backend is chosen once for the process, on the first container write, so
// it is set before any database writes; later calls change nothing written.
func SetWriteEngine(engine string, depth int) {
	writeEngine, uringDepth = engine, depth
}

// requestedEngine is the store io engine SetWriteEngine asked for.
func requestedEngine() string {
	if e := cnst.NormalizeStoreIOEngine(writeEngine); e != "" {
		return e
	}
	return cnst.StoreIOEngineAuto
}

// ioUringQueueDepth is the io-uring queue depth SetWriteEngine asked for, or
// for 0 the machine's (cnst.Tuning.IOUringQueueDepth).
func ioUringQueueDepth() int {
	return cnst.Tuning{IOUringDepth: uringDepth}.IOUringQueueDepth()
}

func getWriteBackend() writeBackend {
	backendOnce.Do(initWriteBackend)
	return selectedBackend
}

func initWriteBackend() {
	engine := requestedEngine()
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
	requested := requestedEngine()
	selected := cnst.StoreIOEngineStdlib
	if selectedBackend != nil {
		selected = selectedBackend.Name()
	}

	depth := 0
	if requested == cnst.StoreIOEngineUring || selected == cnst.StoreIOEngineUring {
		depth = ioUringQueueDepth()
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
