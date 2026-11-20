package diskspace

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
)

// Reserve is the free space DUES keeps on a database's volume while it
// writes there. badger, the database engine, writes its memtables and tables
// through memory-mapped files, and on Linux a write to a mapping the disk
// can no longer back kills the process (SIGBUS) instead of returning an
// error. Two of badger's 64 MiB memtables cover the one being filled and the
// table it is flushed to.
const Reserve = 128 << 20

// ErrLow means a write would leave less than Reserve free. IsNoSpace
// recognises it.
var ErrLow = errors.New("not enough free disk space")

// LowSpaceError says how much was free where.
type LowSpaceError struct {
	Path string
	Free uint64
	Need uint64 // bytes about to be written, without the reserve
}

func (e *LowSpaceError) Error() string {
	if e.Need > 0 {
		return fmt.Sprintf("%v on the volume of %s: %s free, %s to write, and DUES keeps %s free while it writes, because its database engine crashes when the disk fills; free some space and run the command again",
			ErrLow, e.Path, humanize.IBytes(e.Free), humanize.IBytes(e.Need), humanize.IBytes(Reserve))
	}
	return fmt.Sprintf("%v on the volume of %s: %s free, and DUES keeps %s free while it writes, because its database engine crashes when the disk fills; free some space and run the command again",
		ErrLow, e.Path, humanize.IBytes(e.Free), humanize.IBytes(Reserve))
}

func (e *LowSpaceError) Unwrap() error { return ErrLow }

// freeSpace is Free; tests replace it.
var freeSpace = Free

// Check returns a *LowSpaceError when writing need bytes on path's volume
// would leave less than Reserve free.
func Check(path string, need uint64) error {
	free, err := freeSpace(path)
	if err != nil {
		return fmt.Errorf("free space on the volume of %s: %w", path, err)
	}
	if free < need || free-need < Reserve {
		return &LowSpaceError{Path: path, Free: free, Need: need}
	}
	return nil
}

// A Guard checks, as data is written to a volume, that Reserve stays free.
// It asks the filesystem only after checkBytes more bytes or checkInterval,
// so a writer can call it for every block. It is safe for concurrent use.
type Guard struct {
	path string

	mu      sync.Mutex
	pending uint64
	last    time.Time
	err     error
}

const (
	checkBytes    = 16 << 20
	checkInterval = 250 * time.Millisecond
)

// NewGuard returns a guard for path's volume.
func NewGuard(path string) *Guard { return &Guard{path: path} }

// Add accounts for n bytes about to be written, and returns a
// *LowSpaceError once the volume is low. A guard that has reported low space
// keeps reporting it.
func (g *Guard) Add(n int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return g.err
	}
	g.pending += uint64(n)
	if g.pending < checkBytes && time.Since(g.last) < checkInterval {
		return nil
	}
	// The bytes since the last check may not have reached the disk yet, so
	// they count as still to be written.
	err := Check(g.path, g.pending)
	g.pending, g.last = 0, time.Now()
	if _, low := errors.AsType[*LowSpaceError](err); low {
		g.err = err
	}
	return err
}

var (
	guardsMu sync.Mutex
	guards   = map[string]*Guard{}
)

// GuardFor returns the shared guard of path's volume, so every writer to one
// database counts against the same reserve.
func GuardFor(path string) *Guard {
	guardsMu.Lock()
	defer guardsMu.Unlock()
	g := guards[path]
	if g == nil || g.failed() {
		// A guard that reported low space is replaced, so a later write
		// (after space was freed) checks again.
		g = NewGuard(path)
		guards[path] = g
	}
	return g
}

func (g *Guard) failed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err != nil
}
