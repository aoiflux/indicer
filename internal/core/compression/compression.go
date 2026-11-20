package compression

import (
	"bytes"
	"errors"
	"io"

	"github.com/klauspost/compress/zstd"
)

// ErrLimitExceeded means the data decodes to more than the limit allowed.
var ErrLimitExceeded = errors.New("decompressed data exceeds the limit")

// maxWindow bounds the history buffer a frame may ask the decoder for.
// zstd's own levels use at most 8 MiB.
const maxWindow = 64 << 20

func Compress(data []byte, level string) ([]byte, error) {
	encLevel := zstd.SpeedBestCompression
	switch level {
	case "fast":
		encLevel = zstd.SpeedFastest
	case "default":
		encLevel = zstd.SpeedDefault
	case "best", "":
		encLevel = zstd.SpeedBestCompression
	}
	// One encode needs one state; the default allocates one per CPU.
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(encLevel), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer encoder.Close()

	return encoder.EncodeAll(data, nil), nil
}

// DecompressLimited decodes data, failing with ErrLimitExceeded as soon as
// the output would pass limit bytes. Memory stays within the limit and one
// frame's window, whatever the frames declare.
func DecompressLimited(data []byte, limit int64) ([]byte, error) {
	r, err := NewLimitedReader(bytes.NewReader(data), limit)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// LimitedReader decodes a zstd stream and fails with ErrLimitExceeded once
// the output passes its limit.
type LimitedReader struct {
	dec   *zstd.Decoder
	left  int64
	limit int64
}

// NewLimitedReader returns a reader of r's decoded content, at most limit
// bytes of it.
func NewLimitedReader(r io.Reader, limit int64) (*LimitedReader, error) {
	dec, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(maxWindow))
	if err != nil {
		return nil, err
	}
	return &LimitedReader{dec: dec, left: limit, limit: limit}, nil
}

func (l *LimitedReader) Read(p []byte) (int, error) {
	if l.left < int64(len(p)) {
		// Read one byte past the limit to tell "ends at the limit" from
		// "goes on".
		p = p[:max(l.left, 0)+1]
	}
	n, err := l.dec.Read(p)
	if int64(n) > l.left {
		return int(max(l.left, 0)), ErrLimitExceeded
	}
	l.left -= int64(n)
	return n, err
}

// Close releases the decoder.
func (l *LimitedReader) Close() error {
	l.dec.Close()
	return nil
}
