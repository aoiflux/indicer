// Package zstdx holds the zstd codecs the storage engine shares: one
// decoder for the process, built at its first use, and encoders shared by
// level and width, each dropped with the states it holds when its last user
// releases it.
package zstdx

import (
	"runtime"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// MaxDecodedBlock bounds what DecodeAll returns from one call. It decodes
// chunks (at most 64 MiB) and record values, so four times the largest
// chunk leaves room while a crafted few hundred bytes cannot decode to
// gigabytes (CMP-1).
const MaxDecodedBlock = 256 << 20

// decoder is the process decoder. It keeps no goroutines between calls, so
// it is never closed.
var decoder = sync.OnceValues(func() (*zstd.Decoder, error) {
	return zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecoderMaxMemory(MaxDecodedBlock))
})

// DecodeAll decodes the zstd frames in src with the process decoder and
// appends the result to dst.
func DecodeAll(src, dst []byte) ([]byte, error) {
	d, err := decoder()
	if err != nil {
		return nil, err
	}
	return d.DecodeAll(src, dst)
}

// Level returns the zstd level a --compress-level name selects: fast,
// default or best (also the empty name and anything unknown).
func Level(name string) zstd.EncoderLevel {
	switch strings.ToLower(name) {
	case "fast":
		return zstd.SpeedFastest
	case "default":
		return zstd.SpeedDefault
	}
	return zstd.SpeedBestCompression
}

// key names a shared encoder.
type key struct {
	level zstd.EncoderLevel
	width int
}

// shared is one encoder and the number of Encoders using it.
type shared struct {
	enc  *zstd.Encoder
	refs int
}

var (
	mu       sync.Mutex
	encoders = map[key]*shared{}
)

// Encoder is one user's hold on a shared encoder.
type Encoder struct {
	key     key
	s       *shared
	release sync.Once
}

// Acquire returns an encoder at the named level for workers that encode at
// once, and never more than one state per CPU: a worker beyond that waits
// for a free state. Users of the same level and width share one encoder.
//
// An encoder allocates every state at its first use and keeps them until it
// is dropped. A best-compression state is about 50 MB, so one per CPU took
// some 800 MB on a 16-CPU machine even for a single encode (FFI-4); the last
// Release drops the encoder and its states.
func Acquire(level string, workers int) (*Encoder, error) {
	k := key{level: Level(level), width: min(max(workers, 1), runtime.GOMAXPROCS(0))}
	mu.Lock()
	defer mu.Unlock()
	s := encoders[k]
	if s == nil {
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(k.level), zstd.WithEncoderConcurrency(k.width))
		if err != nil {
			return nil, err
		}
		s = &shared{enc: enc}
		encoders[k] = s
	}
	s.refs++
	return &Encoder{key: k, s: s}, nil
}

// EncodeAll encodes src and appends the frame to dst.
func (e *Encoder) EncodeAll(src, dst []byte) []byte {
	return e.s.enc.EncodeAll(src, dst)
}

// Release lets go of the encoder, which must not be used afterwards. The
// last user's release drops it. A second Release does nothing.
func (e *Encoder) Release() {
	e.release.Do(func() {
		mu.Lock()
		defer mu.Unlock()
		e.s.refs--
		if e.s.refs == 0 {
			delete(encoders, e.key)
			// EncodeAll keeps no stream open, so closing cannot fail.
			_ = e.s.enc.Close()
		}
	})
}
