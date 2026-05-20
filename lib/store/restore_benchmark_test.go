package store

import (
	"encoding/binary"
	"os"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/fio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

func BenchmarkRestoreDataPath(b *testing.B) {
	benchmarks := []struct {
		name   string
		chunks int
		start  int64
	}{
		{name: "start_zero/chunks_16", chunks: 16, start: 0},
		{name: "start_mid/chunks_16", chunks: 16, start: cnst.ChonkSize / 2},
		{name: "start_zero/chunks_64", chunks: 64, start: 0},
		{name: "start_mid/chunks_64", chunks: 64, start: cnst.ChonkSize / 2},
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		b.Fatalf("open os.DevNull: %v", err)
	}
	defer devNull.Close()

	oldStderr := os.Stderr
	oldStdout := os.Stdout
	os.Stderr = devNull
	os.Stdout = devNull
	defer func() {
		os.Stderr = oldStderr
		os.Stdout = oldStdout
	}()

	for _, bm := range benchmarks {
		bm := bm
		b.Run(bm.name, func(b *testing.B) {
			meta, db := setupRestoreBenchmarkDataset(b, bm.chunks, bm.start)
			expectedSize := meta.Size

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst, err := os.CreateTemp(b.TempDir(), "restore-bench-*.bin")
				if err != nil {
					b.Fatalf("create temp destination: %v", err)
				}

				if err := restoreData(meta, dst, db); err != nil {
					dst.Close()
					b.Fatalf("restoreData: %v", err)
				}

				if err := dst.Close(); err != nil {
					b.Fatalf("close destination: %v", err)
				}

				info, err := os.Stat(dst.Name())
				if err != nil {
					b.Fatalf("stat destination: %v", err)
				}
				if info.Size() != expectedSize {
					b.Fatalf("unexpected restore size: got %d want %d", info.Size(), expectedSize)
				}
			}
		})
	}
}

func setupRestoreBenchmarkDataset(tb testing.TB, chunks int, start int64) (structs.FileMeta, *badger.DB) {
	tb.Helper()

	db := openTestDB(tb)

	if err := os.MkdirAll(util.BlobPath(db.Opts().Dir), cnst.DirPerm); err != nil {
		tb.Fatalf("create blob dir failed: %v", err)
	}

	eviHash := make([]byte, 32)
	for i := range eviHash {
		eviHash[i] = byte(0xD0 + (i % 16))
	}

	chunkData := make([]byte, cnst.ChonkSize)
	for i := range chunkData {
		chunkData[i] = byte(i % 251)
	}

	for i := 0; i < chunks; i++ {
		restoreIndex := int64(i) * cnst.ChonkSize

		hashSeed := make([]byte, 16)
		binary.BigEndian.PutUint64(hashSeed[:8], uint64(i+101))
		binary.BigEndian.PutUint64(hashSeed[8:], uint64(i+307))
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

	total := int64(chunks) * cnst.ChonkSize
	if start < 0 || start >= total {
		start = 0
	}

	return structs.FileMeta{EviHash: eviHash, Start: start, Size: total - start}, db
}
