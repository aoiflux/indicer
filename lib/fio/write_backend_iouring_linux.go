//go:build linux

package fio

import (
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// errNotSubmitted reports an io-uring write that failed before the kernel
// accepted it, so falling back to a plain write cannot race it.
var errNotSubmitted = errors.New("io-uring write not submitted")

type ioUringWriteBackend struct {
	delegate stdlibWriteBackend
	handleMu sync.Mutex
	handle   ioUringHandle
	initErr  error
	initOnce sync.Once
}

type ioUringHandle interface {
	Close() error
	// SubmitWriteAt writes data at off and returns the bytes written. An
	// error wrapping errNotSubmitted means the kernel never saw the write;
	// after any other error a write may still be in flight.
	SubmitWriteAt(fd int, data []byte, off int64) (int, error)
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

func openIoUringHandle() (ioUringHandle, error) {
	depth := ioUringQueueDepth()
	if depth <= 0 {
		depth = 256
	}
	return newNativeIoUringHandle(uint32(depth))
}

// WriteAt writes data at off through io-uring, continuing after short
// writes. It falls back to a plain positioned write only when the kernel
// never saw the request; once a write may be in flight, an error is returned
// rather than writing the bytes a second time (STO-15).
func (b *ioUringWriteBackend) WriteAt(file *os.File, data []byte, off int64) (int, error) {
	h, err := b.ensureHandle()
	if err != nil {
		warnBackendFallback(cnst.StoreIOEngineUring, err.Error())
		return b.delegate.WriteAt(file, data, off)
	}

	total := 0
	for total < len(data) {
		n, err := h.SubmitWriteAt(int(file.Fd()), data[total:], off+int64(total))
		if errors.Is(err, errNotSubmitted) {
			warnBackendFallback(cnst.StoreIOEngineUring, err.Error())
			m, err := b.delegate.WriteAt(file, data[total:], off+int64(total))
			return total + m, err
		}
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
		total += n
	}
	return total, nil
}

const (
	ioUringOpWrite        = 23
	ioUringEnterGetEvents = 1

	ioUringOffSqRing = 0
	ioUringOffCqRing = 0x8000000
	ioUringOffSqes   = 0x10000000

	ioUringFeatSingleMmap   = 1
	ioUringMaxSubmitRetries = 3
	ioUringDrainBatchSize   = 8
	ioUringMaxBusyRetries   = 100
)

type ioSqringOffsets struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Flags       uint32
	Dropped     uint32
	Array       uint32
	Resv1       uint32
	UserAddr    uint64
}

type ioCqringOffsets struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Overflow    uint32
	Cqes        uint32
	Flags       uint64
	Resv1       uint64
	UserAddr    uint64
}

type ioUringParams struct {
	SqEntries    uint32
	CqEntries    uint32
	Flags        uint32
	SqThreadCpu  uint32
	SqThreadIdle uint32
	Features     uint32
	WqFd         uint32
	Resv         [3]uint32
	SqOff        ioSqringOffsets
	CqOff        ioCqringOffsets
}

type ioUringSqe struct {
	Opcode      uint8
	Flags       uint8
	IoPrio      uint16
	Fd          int32
	Off         uint64
	Addr        uint64
	Len         uint32
	RwFlags     uint32
	UserData    uint64
	BufIndex    uint16
	Personality uint16
	SpliceFdIn  int32
	Addr3       uint64
	Pad2        [1]uint64
}

type ioUringCqe struct {
	UserData uint64
	Res      int32
	Flags    uint32
}

type nativeIoUringHandle struct {
	fd int

	sqRing []byte
	cqRing []byte
	sqes   []byte

	sqHead  *uint32
	sqTail  *uint32
	sqMask  *uint32
	sqArray *uint32

	cqHead  *uint32
	cqTail  *uint32
	cqMask  *uint32
	cqesPtr *ioUringCqe
	sqesPtr *ioUringSqe

	nextUserData uint64
	mu           sync.Mutex
	closed       bool
}

func newNativeIoUringHandle(entries uint32) (*nativeIoUringHandle, error) {
	params := &ioUringParams{}
	fd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, uintptr(entries), uintptr(unsafe.Pointer(params)), 0)
	if errno != 0 {
		return nil, errno
	}

	h := &nativeIoUringHandle{fd: int(fd)}
	if err := h.mapRings(params); err != nil {
		_ = unix.Close(h.fd)
		return nil, err
	}
	return h, nil
}

func (h *nativeIoUringHandle) mapRings(params *ioUringParams) error {
	sqRingSize := int(params.SqOff.Array + params.SqEntries*4)
	cqRingSize := int(params.CqOff.Cqes + params.CqEntries*uint32(unsafe.Sizeof(ioUringCqe{})))
	sqesSize := int(params.SqEntries * uint32(unsafe.Sizeof(ioUringSqe{})))

	ringMapSize := sqRingSize
	if cqRingSize > ringMapSize {
		ringMapSize = cqRingSize
	}

	var err error
	if params.Features&ioUringFeatSingleMmap != 0 {
		h.sqRing, err = unix.Mmap(h.fd, int64(ioUringOffSqRing), ringMapSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return err
		}
		h.cqRing = h.sqRing
	} else {
		h.sqRing, err = unix.Mmap(h.fd, int64(ioUringOffSqRing), sqRingSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return err
		}
		h.cqRing, err = unix.Mmap(h.fd, int64(ioUringOffCqRing), cqRingSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			_ = unix.Munmap(h.sqRing)
			return err
		}
	}

	h.sqes, err = unix.Mmap(h.fd, int64(ioUringOffSqes), sqesSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		if len(h.cqRing) > 0 && (len(h.sqRing) == 0 || unsafe.Pointer(&h.cqRing[0]) != unsafe.Pointer(&h.sqRing[0])) {
			_ = unix.Munmap(h.cqRing)
		}
		if len(h.sqRing) > 0 {
			_ = unix.Munmap(h.sqRing)
		}
		return err
	}

	h.sqHead = (*uint32)(unsafe.Pointer(&h.sqRing[params.SqOff.Head]))
	h.sqTail = (*uint32)(unsafe.Pointer(&h.sqRing[params.SqOff.Tail]))
	h.sqMask = (*uint32)(unsafe.Pointer(&h.sqRing[params.SqOff.RingMask]))
	h.sqArray = (*uint32)(unsafe.Pointer(&h.sqRing[params.SqOff.Array]))

	h.cqHead = (*uint32)(unsafe.Pointer(&h.cqRing[params.CqOff.Head]))
	h.cqTail = (*uint32)(unsafe.Pointer(&h.cqRing[params.CqOff.Tail]))
	h.cqMask = (*uint32)(unsafe.Pointer(&h.cqRing[params.CqOff.RingMask]))
	h.cqesPtr = (*ioUringCqe)(unsafe.Pointer(&h.cqRing[params.CqOff.Cqes]))
	h.sqesPtr = (*ioUringSqe)(unsafe.Pointer(&h.sqes[0]))

	return nil
}

func (h *nativeIoUringHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closeLocked()
}

func (h *nativeIoUringHandle) closeLocked() error {
	if h.closed {
		return nil
	}
	h.closed = true

	if len(h.sqes) > 0 {
		_ = unix.Munmap(h.sqes)
	}
	if len(h.cqRing) > 0 && (len(h.sqRing) == 0 || unsafe.Pointer(&h.cqRing[0]) != unsafe.Pointer(&h.sqRing[0])) {
		_ = unix.Munmap(h.cqRing)
	}
	if len(h.sqRing) > 0 {
		_ = unix.Munmap(h.sqRing)
	}
	return unix.Close(h.fd)
}

// ioUringEnter calls io_uring_enter, repeating it while it is interrupted,
// and for a while when the kernel is briefly out of resources. Repeating is
// safe: a request the kernel has already consumed is not submitted again.
func (h *nativeIoUringHandle) ioUringEnter(toSubmit, minComplete uintptr) error {
	for busy := 0; ; {
		_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(h.fd), toSubmit, minComplete, ioUringEnterGetEvents, 0, 0)
		switch {
		case errno == 0:
			return nil
		case errno == unix.EINTR:
		case (errno == unix.EAGAIN || errno == unix.EBUSY) && busy < ioUringMaxBusyRetries:
			busy++
			time.Sleep(time.Millisecond)
		default:
			return errno
		}
	}
}

func (h *nativeIoUringHandle) SubmitWriteAt(fd int, data []byte, off int64) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	recordIoUringSubmitAttempt()

	if h.closed {
		return 0, fmt.Errorf("%w: io-uring handle is closed", errNotSubmitted)
	}
	if len(data) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, fmt.Errorf("%w: negative offset %d", errNotSubmitted, off)
	}

	var sqHead uint32
	var sqTail uint32
	capacity := *h.sqMask + 1
	for attempt := 0; attempt < ioUringMaxSubmitRetries; attempt++ {
		sqHead = atomic.LoadUint32(h.sqHead)
		sqTail = atomic.LoadUint32(h.sqTail)
		if sqTail-sqHead < capacity {
			break
		}

		// Drain already-available CQEs first; this can refresh producer/consumer
		// visibility and avoid an extra blocking enter() call.
		_, _, _, drainErr := h.drainCompletions(0, ioUringDrainBatchSize)
		if drainErr != nil {
			recordIoUringSubmitError()
			return 0, fmt.Errorf("%w: %v", errNotSubmitted, drainErr)
		}
		sqHead = atomic.LoadUint32(h.sqHead)
		sqTail = atomic.LoadUint32(h.sqTail)
		if sqTail-sqHead < capacity {
			break
		}

		recordIoUringSubmitWait()
		if err := h.ioUringEnter(0, 1); err != nil {
			recordIoUringSubmitError()
			return 0, fmt.Errorf("%w: %v", errNotSubmitted, err)
		}
	}
	if sqTail-sqHead >= capacity {
		recordIoUringQueueFull()
		return 0, fmt.Errorf("%w: io-uring submission queue is full", errNotSubmitted)
	}

	mask := *h.sqMask
	index := sqTail & mask
	sqe := (*ioUringSqe)(unsafe.Pointer(uintptr(unsafe.Pointer(h.sqesPtr)) + uintptr(index)*unsafe.Sizeof(ioUringSqe{})))
	*sqe = ioUringSqe{}
	sqe.Opcode = ioUringOpWrite
	sqe.Fd = int32(fd)
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&data[0])))
	sqe.Len = uint32(min(len(data), 1<<30))
	sqe.Off = uint64(off)

	h.nextUserData++
	userData := h.nextUserData
	sqe.UserData = userData

	arrayEntry := (*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(h.sqArray)) + uintptr(index)*unsafe.Sizeof(uint32(0))))
	*arrayEntry = index
	atomic.StoreUint32(h.sqTail, sqTail+1)

	// From here the kernel may hold a pointer into data. If waiting for the
	// completion fails, the request may still be pending, so the ring is
	// closed, which makes the kernel cancel it, and the handle is never used
	// again; later writes fall back to plain positioned writes.
	broken := func(err error) (int, error) {
		recordIoUringSubmitError()
		_ = h.closeLocked()
		return 0, err
	}
	if err := h.ioUringEnter(1, 1); err != nil {
		return broken(err)
	}

	for {
		found, res, drained, drainErr := h.drainCompletions(userData, ioUringDrainBatchSize)
		if drainErr != nil {
			return broken(drainErr)
		}
		if found {
			if res < 0 {
				recordIoUringSubmitError()
				return 0, unix.Errno(-res)
			}
			recordIoUringCompletion()
			return int(res), nil
		}
		if drained > 0 {
			continue
		}

		recordIoUringSubmitWait()
		if err := h.ioUringEnter(0, 1); err != nil {
			return broken(err)
		}
	}
}

func (h *nativeIoUringHandle) drainCompletions(targetUserData uint64, max int) (found bool, res int32, drained int, err error) {
	if max <= 0 {
		max = 1
	}

	for drained < max {
		cqHead := atomic.LoadUint32(h.cqHead)
		cqTail := atomic.LoadUint32(h.cqTail)
		if cqHead == cqTail {
			break
		}

		cqeIndex := cqHead & *h.cqMask
		cqe := (*ioUringCqe)(unsafe.Pointer(uintptr(unsafe.Pointer(h.cqesPtr)) + uintptr(cqeIndex)*unsafe.Sizeof(ioUringCqe{})))
		cqeRes := cqe.Res
		cqeUserData := cqe.UserData
		atomic.StoreUint32(h.cqHead, cqHead+1)
		drained++

		if targetUserData != 0 && cqeUserData == targetUserData {
			return true, cqeRes, drained, nil
		}
	}

	return false, 0, drained, nil
}
