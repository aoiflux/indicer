package cnst

import (
	"bytes"
	"errors"
	"runtime"
	"testing"

	"github.com/klauspost/compress/zstd"
)

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

// TestNewDecoderIsBounded: the process decoder reads chunks and record
// values, none larger than a 64 MiB chunk, so a few hundred bytes of stored
// data must not decode to gigabytes (CMP-1).
func TestNewDecoderIsBounded(t *testing.T) {
	d, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	ok := zeroFrame(t, 64<<20)
	if out, err := d.DecodeAll(ok, nil); err != nil || len(out) != 64<<20 {
		t.Fatalf("a 64 MiB chunk: %d bytes, %v", len(out), err)
	}
	bomb := zeroFrame(t, MaxDecodedBlock+1<<20)
	if _, err := d.DecodeAll(bomb, nil); !errors.Is(err, zstd.ErrDecoderSizeExceeded) {
		t.Fatalf("%d compressed bytes over the cap: %v, want ErrDecoderSizeExceeded", len(bomb), err)
	}
}

// encodeAlloc returns the bytes allocated by building enc and encoding one
// 64 KiB block with it.
func encodeAlloc(t *testing.T, build func() (*zstd.Encoder, error)) uint64 {
	t.Helper()
	block := bytes.Repeat([]byte("forensic evidence block "), 64<<10/24)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	enc, err := build()
	if err != nil {
		t.Fatal(err)
	}
	enc.EncodeAll(block, nil)
	runtime.ReadMemStats(&after)
	_ = enc.Close()
	return after.TotalAlloc - before.TotalAlloc
}

// TestNewEncoderIsBounded is the FFI-4 regression for the process encoder:
// an encoder allocates every state at its first use, and a best-level state
// is about 50 MB, so the default of one per CPU took hundreds of MB for one
// encode.
func TestNewEncoderIsBounded(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(16))
	const state = 64 << 20 // one best-level state, with room
	if got := encodeAlloc(t, func() (*zstd.Encoder, error) { return NewEncoder("best", 1) }); got > state {
		t.Errorf("one worker: allocated %d bytes, want at most one state (%d)", got, state)
	}
	if got := encodeAlloc(t, func() (*zstd.Encoder, error) { return NewEncoder("best", 3) }); got > 3*state {
		t.Errorf("three workers: allocated %d bytes, want at most three states", got)
	}
	runtime.GOMAXPROCS(2)
	if got := encodeAlloc(t, func() (*zstd.Encoder, error) { return NewEncoder("best", 64) }); got > 2*state {
		t.Errorf("64 workers on 2 CPUs: allocated %d bytes, want at most one state per CPU", got)
	}
}

func TestEncoderLevel(t *testing.T) {
	for name, want := range map[string]zstd.EncoderLevel{
		"fast": zstd.SpeedFastest, "default": zstd.SpeedDefault, "best": zstd.SpeedBestCompression,
		"BEST": zstd.SpeedBestCompression, "": zstd.SpeedBestCompression, "other": zstd.SpeedBestCompression,
	} {
		if got := EncoderLevel(name); got != want {
			t.Errorf("EncoderLevel(%q) = %v, want %v", name, got, want)
		}
	}
}
