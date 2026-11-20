//go:build linux

package fio

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// fakeIoUring stands in for the kernel ring: each call writes at most limit
// bytes, and the call numbered failAt fails with failErr, after writing when
// written is set.
type fakeIoUring struct {
	calls, failAt, limit int
	failErr              error
	written              bool
}

func (*fakeIoUring) Close() error { return nil }

func (f *fakeIoUring) SubmitWriteAt(fd int, data []byte, off int64) (int, error) {
	f.calls++
	if f.limit > 0 && len(data) > f.limit {
		data = data[:f.limit]
	}
	if f.calls == f.failAt {
		if f.written {
			_, _ = unix.Pwrite(fd, data, off)
		}
		return 0, f.failErr
	}
	return unix.Pwrite(fd, data, off)
}

func backendWith(h ioUringHandle) *ioUringWriteBackend {
	b := &ioUringWriteBackend{}
	b.initOnce.Do(func() { b.handle = h })
	return b
}

// TestIoUringWriteNeverDuplicates is the STO-15 regression. When a submitted
// write reported an error (EINTR or EAGAIN from the wait), the backend fell
// back to a plain write at the file position, so the bytes the kernel had
// already written were written a second time and every later offset moved.
func TestIoUringWriteNeverDuplicates(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("0123456789"), 100)
	cases := []struct {
		name    string
		handle  *fakeIoUring
		wantErr bool
	}{
		{"in flight", &fakeIoUring{failAt: 1, failErr: errors.New("wait failed"), written: true}, true},
		{"not submitted", &fakeIoUring{failAt: 1, failErr: fmt.Errorf("%w: queue full", errNotSubmitted)}, false},
		{"short writes", &fakeIoUring{limit: 64}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ctr.blob")
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.Write([]byte("HEAD")); err != nil {
				t.Fatal(err)
			}

			n, err := backendWith(c.handle).WriteAt(f, data, 4)
			if c.wantErr != (err != nil) {
				t.Fatalf("WriteAt returned %d, %v; want error %v", n, err, c.wantErr)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, append([]byte("HEAD"), data...)) {
				t.Fatalf("file holds %d bytes, want the header and the data once (%d)", len(got), 4+len(data))
			}
		})
	}
}

// TestIoUringWriteAtRealRing writes through the kernel ring at explicit
// offsets, out of order, and checks the file.
func TestIoUringWriteAtRealRing(t *testing.T) {
	t.Parallel()
	if err := probeIoUringSupport(); err != nil {
		t.Skipf("io-uring unavailable: %v", err)
	}
	h, err := openIoUringHandle()
	if err != nil {
		t.Skipf("io-uring unavailable: %v", err)
	}
	b := backendWith(h)
	defer h.Close()

	path := filepath.Join(t.TempDir(), "ctr.blob")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	parts := [][]byte{bytes.Repeat([]byte("a"), 5000), bytes.Repeat([]byte("b"), 3), bytes.Repeat([]byte("c"), 70000)}
	offs := []int64{0, 5000, 5003}
	for _, i := range []int{2, 0, 1} {
		n, err := b.WriteAt(f, parts[i], offs[i])
		if err != nil || n != len(parts[i]) {
			t.Fatalf("part %d: wrote %d, %v", i, n, err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.Join(parts, nil)) {
		t.Fatalf("file holds %d bytes that differ from the %d written", len(got), 5000+3+70000)
	}
}
