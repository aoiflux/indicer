package compression

import (
	"github.com/klauspost/compress/zstd"
)

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
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(encLevel))
	if err != nil {
		return nil, err
	}
	defer encoder.Close()

	return encoder.EncodeAll(data, nil), nil
}

func Decompress(data []byte) ([]byte, error) {
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	if err != nil {
		return nil, err
	}
	defer decoder.Close()

	return decoder.DecodeAll(data, nil)
}
