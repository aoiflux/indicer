package service

import (
	"encoding/base64"
	"encoding/binary"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/fio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"
	"sync"
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/klauspost/compress/zstd"
	"github.com/vmihailenco/msgpack/v5"
)

var benchCodecInit sync.Once

func ensureBenchCodec(tb testing.TB) {
	tb.Helper()
	benchCodecInit.Do(func() {
		var err error
		cnst.DECODER, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
		if err != nil {
			tb.Fatalf("decoder init failed: %v", err)
		}
		cnst.ENCODER, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevel(zstd.SpeedBestCompression)))
		if err != nil {
			tb.Fatalf("encoder init failed: %v", err)
		}
	})
}

func setupBenchmarkChunkMapDataset(tb testing.TB, chunks int) (structs.FileMeta, *badger.DB) {
	tb.Helper()
	ensureBenchCodec(tb)

	dir := tb.TempDir()
	key := make([]byte, cnst.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}

	db, err := dbio.ConnectDB(dir, key)
	if err != nil {
		tb.Fatalf("connect db failed: %v", err)
	}
	tb.Cleanup(func() {
		_ = db.Close()
	})

	if err := os.MkdirAll(util.BlobPath(db.Opts().Dir), cnst.DirPerm); err != nil {
		tb.Fatalf("create blob dir failed: %v", err)
	}

	eviHash := make([]byte, 32)
	for i := range eviHash {
		eviHash[i] = byte(0xA0 + (i % 16))
	}

	chunkData := make([]byte, cnst.ChonkSize)
	for i := range chunkData {
		chunkData[i] = byte(i % 251)
	}

	for i := 0; i < chunks; i++ {
		restoreIndex := int64(i) * cnst.ChonkSize

		hashSeed := make([]byte, 16)
		binary.BigEndian.PutUint64(hashSeed[:8], uint64(i+1))
		binary.BigEndian.PutUint64(hashSeed[8:], uint64(i+7))
		chash, err := util.GetChonkHash(hashSeed, cnst.GetHashAlgo())
		if err != nil {
			tb.Fatalf("hash generation failed: %v", err)
		}

		relKey := util.AppendToBytesSlice(cnst.RelationNamespace, eviHash, cnst.DataSeperator, restoreIndex)
		if err := dbio.SetNode(relKey, chash, db); err != nil {
			tb.Fatalf("set relation failed: %v", err)
		}

		ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
		path, err := fio.WriteChonk(db.Opts().Dir, chunkData, ckey, db.Opts().EncryptionKey)
		if err != nil {
			tb.Fatalf("write chunk failed: %v", err)
		}
		stat, err := os.Stat(string(path))
		if err != nil {
			tb.Fatalf("stat chunk failed: %v", err)
		}
		meta := structs.ChonkMetadata{
			Path:         string(path),
			Offset:       0,
			OriginalSize: int64(len(chunkData)),
			StoredSize:   stat.Size(),
			EncodedSize:  stat.Size(),
			Container:    false,
		}
		encodedMeta, err := msgpack.Marshal(meta)
		if err != nil {
			tb.Fatalf("marshal chunk metadata failed: %v", err)
		}
		if err := dbio.SetNode(ckey, encodedMeta, db); err != nil {
			tb.Fatalf("set chunk metadata failed: %v", err)
		}
	}

	meta := structs.FileMeta{
		EviHash: eviHash,
		Start:   0,
		Size:    int64(chunks) * cnst.ChonkSize,
	}
	return meta, db
}

func getChonkMapSequential(meta structs.FileMeta, db *badger.DB) (map[string]int64, error) {
	var dbstart int64
	if meta.Start > 0 {
		dbstart = util.GetDBStartOffset(meta.Start)
	}
	end := meta.Start + meta.Size

	numChunks := int((end - dbstart + cnst.ChonkSize - 1) / cnst.ChonkSize)
	chunkMap := make(map[string]int64, numChunks)
	for restoreIndex := dbstart; restoreIndex < end; restoreIndex += cnst.ChonkSize {
		relKey := util.AppendToBytesSlice(cnst.RelationNamespace, meta.EviHash, cnst.DataSeperator, restoreIndex)
		chash, err := dbio.GetNode(relKey, db)
		if err != nil {
			return nil, err
		}

		ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
		csize, err := dbio.GetChonkSize(restoreIndex, meta.Start, meta.Size, dbstart, end, ckey, db)
		if err != nil {
			return nil, err
		}

		chunkMap[base64.StdEncoding.EncodeToString(chash)] = csize
	}

	return chunkMap, nil
}

func BenchmarkGetChonkMap(b *testing.B) {
	for _, chunks := range []int{16, 64, 256} {
		chunks := chunks

		b.Run("start_zero/chunks_"+itoa(chunks), func(b *testing.B) {
			meta, db := setupBenchmarkChunkMapDataset(b, chunks)
			benchChonkMapCase(b, meta, db, chunks)
		})

		b.Run("start_mid_chunk/chunks_"+itoa(chunks), func(b *testing.B) {
			meta, db := setupBenchmarkChunkMapDataset(b, chunks)
			meta.Start = cnst.ChonkSize / 2
			meta.Size = int64(chunks)*cnst.ChonkSize - meta.Start
			benchChonkMapCase(b, meta, db, chunks)
		})
	}
}

func benchChonkMapCase(b *testing.B, meta structs.FileMeta, db *badger.DB, expectedChunks int) {
	b.Helper()

	b.Run("sequential", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			res, err := getChonkMapSequential(meta, db)
			if err != nil {
				b.Fatalf("sequential benchmark failed: %v", err)
			}
			if len(res) != expectedChunks {
				b.Fatalf("expected %d chunks, got %d", expectedChunks, len(res))
			}
		}
	})

	b.Run("batched_parallel", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			res, err := getChonkMap(meta, db)
			if err != nil {
				b.Fatalf("batched+parallel benchmark failed: %v", err)
			}
			if len(res) != expectedChunks {
				b.Fatalf("expected %d chunks, got %d", expectedChunks, len(res))
			}
		}
	})
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}

	neg := v < 0
	if neg {
		v = -v
	}

	buf := [20]byte{}
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + (v % 10))
		v /= 10
	}

	if neg {
		i--
		buf[i] = '-'
	}

	return string(buf[i:])
}
