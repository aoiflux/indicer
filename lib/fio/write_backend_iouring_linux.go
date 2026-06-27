//go:build linux

package fio

import (
	"errors"
	"indicer/lib/cnst"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

func openIoUringHandle() (ioUringHandle, error) {
	return newNativeIoUringHandle(256)
}

func (b *ioUringWriteBackend) WriteBlob(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, cnst.FilePerm)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}

	h, hErr := b.ensureHandle()
	if hErr != nil {
		warnBackendFallback(cnst.StoreIOEngineUring, hErr.Error())
		n, wErr := b.delegate.WriteToFile(f, data)
		if wErr != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return wErr
		}
		if n != len(data) {
			_ = f.Close()
			_ = os.Remove(path)
			return io.ErrShortWrite
		}
	} else {
		n, wErr := h.SubmitWrite(int(f.Fd()), data)
		if wErr != nil {
			warnBackendFallback(cnst.StoreIOEngineUring, wErr.Error())
			n, fallbackErr := b.delegate.WriteToFile(f, data)
			if fallbackErr != nil {
				_ = f.Close()
				_ = os.Remove(path)
				return fallbackErr
			}
			if n != len(data) {
				_ = f.Close()
				_ = os.Remove(path)
				return io.ErrShortWrite
			}
		} else if n != len(data) {
			_ = f.Close()
			_ = os.Remove(path)
			return io.ErrShortWrite
		}
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func (b *ioUringWriteBackend) WriteToFile(file *os.File, data []byte) (int, error) {
	h, err := b.ensureHandle()
	if err != nil {
		warnBackendFallback(cnst.StoreIOEngineUring, err.Error())
		return b.delegate.WriteToFile(file, data)
	}

	n, submitErr := h.SubmitWrite(int(file.Fd()), data)
	if submitErr != nil {
		warnBackendFallback(cnst.StoreIOEngineUring, submitErr.Error())
		return b.delegate.WriteToFile(file, data)
	}
	if n != len(data) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

const (
	ioUringOpWrite        = 23
	ioUringEnterGetEvents = 1

	ioUringOffSqRing = 0
	ioUringOffCqRing = 0x8000000
	ioUringOffSqes   = 0x10000000

	ioUringFeatSingleMmap = 1
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

func (h *nativeIoUringHandle) SubmitWrite(fd int, data []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return 0, errors.New("io-uring handle is closed")
	}
	if len(data) == 0 {
		return 0, nil
	}

	sqHead := atomic.LoadUint32(h.sqHead)
	sqTail := atomic.LoadUint32(h.sqTail)
	if sqTail-sqHead >= *h.sqMask+1 {
		return 0, errors.New("io-uring submission queue is full")
	}

	mask := *h.sqMask
	index := sqTail & mask
	sqe := (*ioUringSqe)(unsafe.Pointer(uintptr(unsafe.Pointer(h.sqesPtr)) + uintptr(index)*unsafe.Sizeof(ioUringSqe{})))
	*sqe = ioUringSqe{}
	sqe.Opcode = ioUringOpWrite
	sqe.Fd = int32(fd)
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&data[0])))
	sqe.Len = uint32(len(data))
	sqe.Off = ^uint64(0) // use current file offset (like write())

	h.nextUserData++
	userData := h.nextUserData
	sqe.UserData = userData

	arrayEntry := (*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(h.sqArray)) + uintptr(index)*unsafe.Sizeof(uint32(0))))
	*arrayEntry = index
	atomic.StoreUint32(h.sqTail, sqTail+1)

	if _, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(h.fd), 1, 1, ioUringEnterGetEvents, 0, 0); errno != 0 {
		return 0, errno
	}

	for {
		cqHead := atomic.LoadUint32(h.cqHead)
		cqTail := atomic.LoadUint32(h.cqTail)
		if cqHead == cqTail {
			if _, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(h.fd), 0, 1, ioUringEnterGetEvents, 0, 0); errno != 0 {
				return 0, errno
			}
			continue
		}

		cqeIndex := cqHead & *h.cqMask
		cqe := (*ioUringCqe)(unsafe.Pointer(uintptr(unsafe.Pointer(h.cqesPtr)) + uintptr(cqeIndex)*unsafe.Sizeof(ioUringCqe{})))
		res := cqe.Res
		atomic.StoreUint32(h.cqHead, cqHead+1)

		if cqe.UserData != userData {
			continue
		}
		if res < 0 {
			return 0, unix.Errno(-res)
		}
		return int(res), nil
	}
}
