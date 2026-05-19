package store

import (
	"fmt"
	"os"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/klauspost/compress/zstd"
)

const revRelSyntheticAppendShards = 16

func BenchmarkProcessRevRel(b *testing.B) {
	ensureBenchmarkCodecs(b)

	oldQuickOpt := cnst.QUICKOPT
	cnst.QUICKOPT = true
	defer func() {
		cnst.QUICKOPT = oldQuickOpt
	}()

	b.Run("existing_same_file_noop", func(b *testing.B) {
		db := openRevRelBenchDB(b)
		defer db.Close()

		index := int64(0)
		chash := []byte("bench-chash-fixed")
		fhash := []byte("bench-file-fixed")

		seedBatch := db.NewWriteBatch()
		if err := dbio.SetReverseRelationAppendMember(chash, index, fhash, seedBatch); err != nil {
			b.Fatalf("seed reverse relation: %v", err)
		}
		if err := seedBatch.Flush(); err != nil {
			b.Fatalf("seed flush: %v", err)
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			batch := db.NewWriteBatch()
			err := processRevRel(index, fhash, chash, batch, nil)
			if err != nil {
				batch.Cancel()
				b.Fatalf("processRevRel noop path failed: %v", err)
			}
			if err := batch.Flush(); err != nil {
				b.Fatalf("flush noop path failed: %v", err)
			}
		}
	})

	b.Run("existing_new_file_write", func(b *testing.B) {
		db := openRevRelBenchDB(b)
		defer db.Close()

		index := int64(0)
		chash := []byte("bench-chash-fixed")

		seedBatch := db.NewWriteBatch()
		if err := dbio.SetReverseRelationAppendMember(chash, index, []byte("seed-file"), seedBatch); err != nil {
			b.Fatalf("seed reverse relation: %v", err)
		}
		if err := seedBatch.Flush(); err != nil {
			b.Fatalf("seed flush: %v", err)
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			batch := db.NewWriteBatch()
			fhash := []byte(fmt.Sprintf("bench-file-%d", i))
			err := processRevRel(index, fhash, chash, batch, nil)
			if err != nil {
				batch.Cancel()
				b.Fatalf("processRevRel write path failed: %v", err)
			}
			if err := batch.Flush(); err != nil {
				b.Fatalf("flush write path failed: %v", err)
			}
		}
	})

	b.Run("synthetic_append_shard_new_file_write", func(b *testing.B) {
		db := openRevRelBenchDB(b)
		defer db.Close()

		index := int64(0)
		chash := []byte("bench-chash-fixed")

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			batch := db.NewWriteBatch()
			fhash := []byte(fmt.Sprintf("bench-file-%d", i))
			if err := setSyntheticAppendMember(batch, chash, index, int64(i), fhash); err != nil {
				batch.Cancel()
				b.Fatalf("synthetic append write failed: %v", err)
			}
			if err := batch.Flush(); err != nil {
				b.Fatalf("synthetic append flush failed: %v", err)
			}
		}
	})

	b.Run("append_collect_32_members", func(b *testing.B) {
		db := openRevRelBenchDB(b)
		defer db.Close()

		index := int64(0)
		chash := []byte("bench-chash-fixed")
		revRelKey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator, index)

		seedBatch := db.NewWriteBatch()
		for i := 0; i < 32; i++ {
			fhash := []byte(fmt.Sprintf("seed-file-%02d", i))
			if err := dbio.SetReverseRelationAppendMember(chash, index, fhash, seedBatch); err != nil {
				b.Fatalf("seed reverse relation append: %v", err)
			}
		}
		if err := seedBatch.Flush(); err != nil {
			b.Fatalf("seed reverse relation append flush: %v", err)
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			revMap, err := dbio.GetReverseRelationNode(revRelKey, db)
			if err != nil {
				b.Fatalf("append collect failed: %v", err)
			}
			if len(revMap) != 32 {
				b.Fatalf("unexpected append member count: %d", len(revMap))
			}
		}
	})

	b.Run("synthetic_append_shard_collect_32_members", func(b *testing.B) {
		db := openRevRelBenchDB(b)
		defer db.Close()

		index := int64(0)
		chash := []byte("bench-chash-fixed")

		seedBatch := db.NewWriteBatch()
		for i := 0; i < 32; i++ {
			fhash := []byte(fmt.Sprintf("seed-file-%02d", i))
			if err := setSyntheticAppendMember(seedBatch, chash, index, int64(i), fhash); err != nil {
				b.Fatalf("seed synthetic append member: %v", err)
			}
		}
		if err := seedBatch.Flush(); err != nil {
			b.Fatalf("seed synthetic append flush: %v", err)
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			revMap, err := collectSyntheticAppendMembers(chash, index, db)
			if err != nil {
				b.Fatalf("synthetic append collect failed: %v", err)
			}
			if len(revMap) != 32 {
				b.Fatalf("unexpected synthetic append member count: %d", len(revMap))
			}
		}
	})
}

func ensureBenchmarkCodecs(b *testing.B) {
	if cnst.DECODER == nil {
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
		if err != nil {
			b.Fatalf("init zstd decoder: %v", err)
		}
		cnst.DECODER = decoder
	}
	if cnst.ENCODER == nil {
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevel(zstd.SpeedBestCompression)))
		if err != nil {
			b.Fatalf("init zstd encoder: %v", err)
		}
		cnst.ENCODER = encoder
	}
}

func openRevRelBenchDB(b *testing.B) *badger.DB {
	tempDir := b.TempDir()
	encryptionKey := make([]byte, cnst.KeySize)
	for i := range encryptionKey {
		encryptionKey[i] = byte(i + 1)
	}

	db, err := dbio.ConnectDB(tempDir, encryptionKey)
	if err != nil {
		b.Fatalf("open badger: %v", err)
	}
	if err := os.MkdirAll(util.BlobPath(db.Opts().Dir), cnst.DirPerm); err != nil {
		b.Fatalf("create blob dir: %v", err)
	}
	return db
}

func setSyntheticAppendMember(batch *badger.WriteBatch, chash []byte, index, memberID int64, fhash []byte) error {
	key := syntheticAppendMemberKey(chash, index, memberID, fhash)
	return batch.Set(key, fhash)
}

func syntheticAppendMemberKey(chash []byte, index, memberID int64, fhash []byte) []byte {
	shard := syntheticAppendShard(fhash)
	return util.AppendToBytesSlice("revapp:", chash, cnst.DataSeperator, index, cnst.DataSeperator, shard, cnst.DataSeperator, memberID)
}

func syntheticAppendShard(fhash []byte) int {
	if len(fhash) == 0 {
		return 0
	}
	return int(fhash[len(fhash)-1]) % revRelSyntheticAppendShards
}

func collectSyntheticAppendMembers(chash []byte, index int64, db *badger.DB) (map[string]struct{}, error) {
	revMap := make(map[string]struct{})
	prefix := util.AppendToBytesSlice("revapp:", chash, cnst.DataSeperator, index, cnst.DataSeperator)

	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 128
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			value, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}
			revMap[string(value)] = struct{}{}
		}

		return nil
	})

	return revMap, err
}
