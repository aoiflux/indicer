// Package testutil holds test helpers shared across DUES packages: I/O fault
// injectors, file and golden helpers, environment gates, subprocess scenarios
// and leak checks.
//
// It imports no DUES packages, so any package's tests can use it without an
// import cycle.
package testutil

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// ErrInjected is the error the fault injectors return unless told otherwise.
var ErrInjected = errors.New("testutil: injected fault")

// FailingReaderAt wraps an io.ReaderAt and fails every read that touches
// offset FailAt or beyond. Err defaults to ErrInjected.
type FailingReaderAt struct {
	R      io.ReaderAt
	FailAt int64
	Err    error
}

func (f *FailingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	err := f.Err
	if err == nil {
		err = ErrInjected
	}
	if off >= f.FailAt {
		return 0, err
	}
	if end := off + int64(len(p)); end > f.FailAt {
		n, rerr := f.R.ReadAt(p[:f.FailAt-off], off)
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return n, rerr
		}
		return n, err
	}
	return f.R.ReadAt(p, off)
}

// ShortReaderAt wraps an io.ReaderAt and returns at most Max bytes per call,
// with a nil error, which io.ReaderAt forbids. It exposes callers that trust n
// without checking it.
type ShortReaderAt struct {
	R   io.ReaderAt
	Max int
}

func (s *ShortReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if s.Max > 0 && len(p) > s.Max {
		p = p[:s.Max]
	}
	n, err := s.R.ReadAt(p, off)
	if errors.Is(err, io.EOF) && n > 0 {
		err = nil
	}
	return n, err
}

// ENOSPCWriter accepts After bytes and then fails every write with the
// platform's disk-full error (ENOSPC, or ERROR_DISK_FULL on Windows). The
// write that crosses the limit is short: it stores the bytes that fit.
type ENOSPCWriter struct {
	W     io.Writer
	After int64

	mu      sync.Mutex
	written int64
}

// NewENOSPCWriter returns a writer that runs out of space after n bytes.
func NewENOSPCWriter(w io.Writer, n int64) *ENOSPCWriter {
	return &ENOSPCWriter{W: w, After: n}
}

func (e *ENOSPCWriter) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	room := e.After - e.written
	if room <= 0 {
		return 0, ErrNoSpace()
	}
	if int64(len(p)) <= room {
		n, err := e.W.Write(p)
		e.written += int64(n)
		return n, err
	}
	n, err := e.W.Write(p[:room])
	e.written += int64(n)
	if err != nil {
		return n, err
	}
	return n, ErrNoSpace()
}

// Written reports how many bytes reached the underlying writer.
func (e *ENOSPCWriter) Written() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.written
}

// ShortWriter writes at most Max bytes per call and reports a nil error,
// which io.Writer forbids. It exposes callers that ignore n.
type ShortWriter struct {
	W   io.Writer
	Max int
}

func (s *ShortWriter) Write(p []byte) (int, error) {
	if s.Max > 0 && len(p) > s.Max {
		p = p[:s.Max]
	}
	return s.W.Write(p)
}

// ErrCloser wraps a reader or writer whose Close fails with Err (default
// ErrInjected) after closing the inner value, if it is an io.Closer.
type ErrCloser struct {
	Inner any
	Err   error

	closed atomic.Int32
}

func (e *ErrCloser) Read(p []byte) (int, error) {
	r, ok := e.Inner.(io.Reader)
	if !ok {
		return 0, fmt.Errorf("testutil: %T is not a reader", e.Inner)
	}
	return r.Read(p)
}

func (e *ErrCloser) Write(p []byte) (int, error) {
	w, ok := e.Inner.(io.Writer)
	if !ok {
		return 0, fmt.Errorf("testutil: %T is not a writer", e.Inner)
	}
	return w.Write(p)
}

func (e *ErrCloser) Close() error {
	e.closed.Add(1)
	if c, ok := e.Inner.(io.Closer); ok {
		_ = c.Close()
	}
	if e.Err != nil {
		return e.Err
	}
	return ErrInjected
}

// Closes reports how many times Close was called.
func (e *ErrCloser) Closes() int { return int(e.closed.Load()) }

// SlowReaderAt delays every read by Delay, so tests can exercise timeouts and
// cancellation without a real slow device.
type SlowReaderAt struct {
	R     io.ReaderAt
	Delay time.Duration
}

func (s *SlowReaderAt) ReadAt(p []byte, off int64) (int, error) {
	time.Sleep(s.Delay)
	return s.R.ReadAt(p, off)
}

// BlockingReaderAt blocks every read until Release is closed, then serves it.
// Started, if non-nil, receives one value per read that has begun blocking.
type BlockingReaderAt struct {
	R       io.ReaderAt
	Release chan struct{}
	Started chan struct{}
}

func (b *BlockingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if b.Started != nil {
		select {
		case b.Started <- struct{}{}:
		default:
		}
	}
	<-b.Release
	return b.R.ReadAt(p, off)
}
