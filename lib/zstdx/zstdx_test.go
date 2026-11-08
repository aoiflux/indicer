package zstdx

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// live returns the number of shared encoders.
func live() int {
	mu.Lock()
	defer mu.Unlock()
	return len(encoders)
}

// zeroFrame returns a small zstd stream that decodes to n zero bytes,
// written without holding n bytes in memory.
func zeroFrame(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		t.Fatal(err)
	}
	zeros := make([]byte, 1<<20)
	for left := n; left > 0; left -= len(zeros) {
		if _, err := w.Write(zeros[:min(left, len(zeros))]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestDecodeAllIsBounded: the process decoder reads chunks and record
// values, none larger than a 64 MiB chunk, so a few hundred bytes of stored
// data must not decode to gigabytes (CMP-1).
func TestDecodeAllIsBounded(t *testing.T) {
	t.Parallel()
	ok := zeroFrame(t, 64<<20)
	if out, err := DecodeAll(ok, nil); err != nil || len(out) != 64<<20 {
		t.Fatalf("a 64 MiB chunk: %d bytes, %v", len(out), err)
	}
	bomb := zeroFrame(t, MaxDecodedBlock+1<<20)
	if _, err := DecodeAll(bomb, nil); !errors.Is(err, zstd.ErrDecoderSizeExceeded) {
		t.Fatalf("%d compressed bytes over the cap: %v, want ErrDecoderSizeExceeded", len(bomb), err)
	}
}

// encodeAlloc returns the bytes allocated by acquiring an encoder and
// encoding one 64 KiB block with it.
func encodeAlloc(t *testing.T, level string, workers int) uint64 {
	t.Helper()
	block := bytes.Repeat([]byte("forensic evidence block "), 64<<10/24)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	enc, err := Acquire(level, workers)
	if err != nil {
		t.Fatal(err)
	}
	enc.EncodeAll(block, nil)
	runtime.ReadMemStats(&after)
	enc.Release()
	return after.TotalAlloc - before.TotalAlloc
}

// TestAcquireIsBounded is the FFI-4 regression: an encoder allocates every
// state at its first use, and a best-level state is about 50 MB, so the
// default of one per CPU took hundreds of MB for one encode.
func TestAcquireIsBounded(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(16))
	const state = 64 << 20 // one best-level state, with room
	if got := encodeAlloc(t, "best", 1); got > state {
		t.Errorf("one worker: allocated %d bytes, want at most one state (%d)", got, state)
	}
	if got := encodeAlloc(t, "best", 3); got > 3*state {
		t.Errorf("three workers: allocated %d bytes, want at most three states", got)
	}
	runtime.GOMAXPROCS(2)
	if got := encodeAlloc(t, "best", 64); got > 2*state {
		t.Errorf("64 workers on 2 CPUs: allocated %d bytes, want at most one state per CPU", got)
	}
}

func TestLevel(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]zstd.EncoderLevel{
		"fast": zstd.SpeedFastest, "default": zstd.SpeedDefault, "best": zstd.SpeedBestCompression,
		"BEST": zstd.SpeedBestCompression, "": zstd.SpeedBestCompression, "other": zstd.SpeedBestCompression,
	} {
		if got := Level(name); got != want {
			t.Errorf("Level(%q) = %v, want %v", name, got, want)
		}
	}
}

func mustAcquire(t *testing.T, level string, workers int) *Encoder {
	t.Helper()
	e, err := Acquire(level, workers)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestAcquireSharesAndDrops: users of one level and width share an encoder,
// which goes once the last of them lets go, and a second Release by one
// user cannot take another's.
func TestAcquireSharesAndDrops(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(4))
	a := mustAcquire(t, "fast", 2)
	b := mustAcquire(t, "fast", 2)
	wide := mustAcquire(t, "fast", 64) // 4 states: a width of its own
	best := mustAcquire(t, "best", 2)
	if a.s != b.s {
		t.Fatal("one level and width got two encoders")
	}
	if wide.s == a.s || best.s == a.s {
		t.Fatal("another width or level shares the encoder")
	}
	if n := live(); n != 3 {
		t.Fatalf("%d encoders, want 3", n)
	}

	a.Release()
	a.Release()
	if live() != 3 {
		t.Fatal("the encoder went while b still holds it")
	}
	if out, err := DecodeAll(b.EncodeAll([]byte("still here"), nil), nil); err != nil || string(out) != "still here" {
		t.Fatalf("b after a let go: %q, %v", out, err)
	}
	b.Release()
	wide.Release()
	best.Release()
	if n := live(); n != 0 {
		t.Fatalf("%d encoders after every release, want 0", n)
	}

	c := mustAcquire(t, "fast", 2)
	defer c.Release()
	if c.s == a.s {
		t.Fatal("a dropped encoder came back")
	}
	b.Release()
	if live() != 1 {
		t.Fatal("a stale Release dropped a new user's encoder")
	}
}

// TestAcquireConcurrent: users that come and go at once each get a working
// encoder, and none is left when they are all done.
func TestAcquireConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := range 32 {
		wg.Go(func() {
			e, err := Acquire([]string{"fast", "default"}[i%2], 1+i%3)
			if err != nil {
				errs <- err
				return
			}
			defer e.Release()
			msg := fmt.Appendf(nil, "user %d", i)
			out, err := DecodeAll(e.EncodeAll(msg, nil), nil)
			if err != nil || !bytes.Equal(out, msg) {
				errs <- fmt.Errorf("user %d: %q, %v", i, out, err)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := live(); n != 0 {
		t.Fatalf("%d encoders left, want 0", n)
	}
}
