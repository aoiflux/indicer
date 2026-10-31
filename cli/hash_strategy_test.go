package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"indicer/internal/console"
	"indicer/internal/testutil"
	"io"
	"os"
	"path/filepath"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/klauspost/compress/zstd"
)

func ensureZstdReadyForTests(t *testing.T) {
	t.Helper()
	if cnst.DECODER == nil {
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
		if err != nil {
			t.Fatalf("init zstd decoder: %v", err)
		}
		cnst.DECODER = decoder
	}
	if cnst.ENCODER == nil {
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevel(zstd.SpeedDefault)))
		if err != nil {
			t.Fatalf("init zstd encoder: %v", err)
		}
		cnst.ENCODER = encoder
	}
}

func collectEvidenceHashState(t *testing.T, dbPath string, key []byte, rawHash []byte) (string, []byte) {
	t.Helper()

	h, err := openTestDB(dbPath, key)
	if err != nil {
		t.Fatalf("ConnectDB: %v", err)
	}
	defer h.Close()
	db := h.DB

	rhash := base64.StdEncoding.EncodeToString(rawHash)

	fid, err := dbio.GuessFileType(rhash, db)
	if err != nil {
		t.Fatalf("GuessFileType: %v", err)
	}
	evi, err := dbio.GetEvidenceFile(fid, db)
	if err != nil {
		t.Fatalf("GetEvidenceFile: %v", err)
	}
	if evi.FileHash == "" {
		t.Fatal("expected committed evidence file hash to be set")
	}

	lookupKey := util.AppendToBytesSlice(cnst.EviFileHashLookupNamespace, evi.FileHash)
	mappedIDs, err := dbio.GetHashLookupUUIDsByKey(lookupKey, db)
	if err != nil {
		t.Fatalf("GetHashLookupUUIDsByKey: %v", err)
	}
	found := false
	for _, mappedID := range mappedIDs {
		if bytes.Equal(mappedID, fid) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected hash lookup to contain committed evidence id")
	}

	return evi.FileHash, fid
}

func computeExpectedHochoHashForTest(filePath string) (string, error) {
	handle, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer handle.Close()

	if _, err := handle.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	hasher := cnst.GetHashAlgo(true)
	buf := make([]byte, cnst.ChonkSize)
	for {
		n, readErr := handle.Read(buf)
		if n > 0 {
			chunkHash, hashErr := util.GetChonkHash(buf[:n], cnst.GetHashAlgo(true))
			if hashErr != nil {
				return "", hashErr
			}
			var lenPrefix [4]byte
			binary.BigEndian.PutUint32(lenPrefix[:], uint32(len(chunkHash)))
			if _, err := hasher.Write(lenPrefix[:]); err != nil {
				return "", err
			}
			if _, err := hasher.Write(chunkHash); err != nil {
				return "", err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}

	return base64.StdEncoding.EncodeToString(hasher.Sum(nil)), nil
}

func TestStoreHashStrategyEvidenceParity(t *testing.T) {
	ensureZstdReadyForTests(t)

	originalStrategy := cnst.StoreHashStrategy
	t.Cleanup(func() {
		cnst.StoreHashStrategy = originalStrategy
	})

	inputPath := filepath.Join(t.TempDir(), "sample.bin")
	chunk := []byte("hash-strategy-parity")
	data := bytes.Repeat(chunk, 8192)
	if err := os.WriteFile(inputPath, data, 0o600); err != nil {
		t.Fatalf("write input file: %v", err)
	}

	// Compute expected evidence file hash once.
	handle, err := os.Open(inputPath)
	if err != nil {
		t.Fatalf("open input for hash: %v", err)
	}
	expectedHashRaw, err := testutil.HashFile(handle, cnst.GetHashAlgo(true))
	closeErr := handle.Close()
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("close input handle: %v", closeErr)
	}
	expectedHash := base64.StdEncoding.EncodeToString(expectedHashRaw)

	key := []byte("01234567890123456789012345678901")
	originalChunkSize := cnst.ChonkSize
	util.SetChonkSize(64)
	t.Cleanup(func() {
		cnst.ChonkSize = originalChunkSize
	})
	expectedHocho, err := computeExpectedHochoHashForTest(inputPath)
	if err != nil {
		t.Fatalf("computeSyncHochoHash: %v", err)
	}

	dbPathSync := filepath.Join(t.TempDir(), "db-sync")
	cnst.StoreHashStrategy = cnst.SyncHashStrategy
	if err := StoreData(t.Context(), optsFor(dbPathSync, 64, key), inputPath, false, true, false, false); err != nil {
		t.Fatalf("StoreData sync: %v", err)
	}
	syncHash, _ := collectEvidenceHashState(t, dbPathSync, key, expectedHashRaw)

	dbPathAsync := filepath.Join(t.TempDir(), "db-async")
	cnst.StoreHashStrategy = cnst.AsyncHashStrategy
	if err := StoreData(t.Context(), optsFor(dbPathAsync, 64, key), inputPath, false, true, false, false); err != nil {
		t.Fatalf("StoreData async: %v", err)
	}
	asyncHash, _ := collectEvidenceHashState(t, dbPathAsync, key, expectedHashRaw)

	if syncHash != expectedHash {
		t.Fatalf("sync committed %q, want %q", syncHash, expectedHash)
	}
	if asyncHash != expectedHash {
		t.Fatalf("async committed %q, want %q", asyncHash, expectedHash)
	}
	if syncHash != asyncHash {
		t.Fatalf("strategy mismatch: sync=%q async=%q", syncHash, asyncHash)
	}

	dbPathHocho := filepath.Join(t.TempDir(), "db-hocho")
	cnst.StoreHashStrategy = cnst.HochoHashStrategy
	if err := StoreData(t.Context(), optsFor(dbPathHocho, 64, key), inputPath, false, true, false, false); err != nil {
		t.Fatalf("StoreData hocho: %v", err)
	}
	hochoRaw, err := base64.StdEncoding.DecodeString(expectedHocho)
	if err != nil {
		t.Fatalf("decode expected hocho: %v", err)
	}
	hochoHash, _ := collectEvidenceHashState(t, dbPathHocho, key, hochoRaw)
	if hochoHash != expectedHocho {
		t.Fatalf("hocho committed %q, want %q", hochoHash, expectedHocho)
	}
}

func TestStoreHashStrategyKeyModelContract(t *testing.T) {
	ensureZstdReadyForTests(t)

	originalStrategy := cnst.StoreHashStrategy
	originalChunkSize := cnst.ChonkSize
	util.SetChonkSize(64)
	t.Cleanup(func() {
		cnst.StoreHashStrategy = originalStrategy
		cnst.ChonkSize = originalChunkSize
	})

	inputPath := filepath.Join(t.TempDir(), "key-model.bin")
	data := bytes.Repeat([]byte("key-model-contract"), 4096)
	if err := os.WriteFile(inputPath, data, 0o600); err != nil {
		t.Fatalf("write input file: %v", err)
	}

	handle, err := os.Open(inputPath)
	if err != nil {
		t.Fatalf("open input for sync hash: %v", err)
	}
	syncRaw, err := testutil.HashFile(handle, cnst.GetHashAlgo(true))
	closeErr := handle.Close()
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("close input handle: %v", closeErr)
	}
	syncEncoded := base64.StdEncoding.EncodeToString(syncRaw)

	hochoEncoded, err := computeExpectedHochoHashForTest(inputPath)
	if err != nil {
		t.Fatalf("computeSyncHochoHash: %v", err)
	}
	hochoRaw, err := base64.StdEncoding.DecodeString(hochoEncoded)
	if err != nil {
		t.Fatalf("decode hocho hash: %v", err)
	}

	key := []byte("01234567890123456789012345678901")
	strategies := []string{cnst.SyncHashStrategy, cnst.AsyncHashStrategy, cnst.HochoHashStrategy}

	for _, strategy := range strategies {
		strategy := strategy
		t.Run(strategy, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "db-"+strategy)
			cnst.StoreHashStrategy = strategy
			if err := StoreData(t.Context(), optsFor(dbPath, 64, key), inputPath, false, true, false, false); err != nil {
				t.Fatalf("StoreData %s: %v", strategy, err)
			}

			expectedEvidenceRaw := syncRaw
			expectedEvidenceHash := syncEncoded
			if strategy == cnst.HochoHashStrategy {
				expectedEvidenceRaw = hochoRaw
				expectedEvidenceHash = hochoEncoded
			}

			committedHash, evidenceID := collectEvidenceHashState(t, dbPath, key, expectedEvidenceRaw)
			if committedHash != expectedEvidenceHash {
				t.Fatalf("committed evidence hash mismatch for %s: got=%q want=%q", strategy, committedHash, expectedEvidenceHash)
			}

			if !bytes.HasPrefix(evidenceID, []byte(cnst.EviFileNamespace)) {
				t.Fatalf("evidence key must be E namespace for %s: %q", strategy, evidenceID)
			}
			hashShapedEvidenceKey := util.AppendToBytesSlice(cnst.EviFileNamespace, expectedEvidenceRaw)
			if bytes.Equal(evidenceID, hashShapedEvidenceKey) {
				t.Fatalf("evidence key unexpectedly hash-addressed for %s", strategy)
			}

			partitionHash, indexedHash := attachSyntheticPartitionAndIndexed(t, dbPath, key, evidenceID, inputPath, strategy, strategy)

			h, err := openTestDB(dbPath, key)
			if err != nil {
				t.Fatalf("ConnectDB: %v", err)
			}
			defer h.Close()
			db := h.DB

			partitionRaw, err := base64.StdEncoding.DecodeString(partitionHash)
			if err != nil {
				t.Fatalf("decode partition hash: %v", err)
			}
			indexedRaw, err := base64.StdEncoding.DecodeString(indexedHash)
			if err != nil {
				t.Fatalf("decode indexed hash: %v", err)
			}

			partitionKey := util.AppendToBytesSlice(cnst.PartiFileNamespace, partitionRaw)
			if err := dbio.PingNode(partitionKey, db); err != nil {
				t.Fatalf("partition key must be namespaced hash for %s: %v", strategy, err)
			}
			indexedKey := util.AppendToBytesSlice(cnst.IdxFileNamespace, indexedRaw)
			if err := dbio.PingNode(indexedKey, db); err != nil {
				t.Fatalf("indexed key must be namespaced hash for %s: %v", strategy, err)
			}

			partitionLookupKey := util.AppendToBytesSlice(cnst.EviFileHashLookupNamespace, partitionHash)
			if err := dbio.PingNode(partitionLookupKey, db); !errors.Is(err, badger.ErrKeyNotFound) {
				t.Fatalf("partition hash must not be in evidence hash lookup namespace for %s", strategy)
			}
			indexedLookupKey := util.AppendToBytesSlice(cnst.EviFileHashLookupNamespace, indexedHash)
			if err := dbio.PingNode(indexedLookupKey, db); !errors.Is(err, badger.ErrKeyNotFound) {
				t.Fatalf("indexed hash must not be in evidence hash lookup namespace for %s", strategy)
			}

			partitionHashIDs, err := dbio.GetHashLookupUUIDsByKey(util.AppendToBytesSlice(cnst.PartiFileHashLookupNamespace, partitionHash), db)
			if err != nil {
				t.Fatalf("partition hash lookup should be populated for %s: %v", strategy, err)
			}
			if len(partitionHashIDs) == 0 {
				t.Fatalf("partition hash lookup should contain at least one id for %s", strategy)
			}

			indexedHashIDs, err := dbio.GetHashLookupUUIDsByKey(util.AppendToBytesSlice(cnst.IdxFileHashLookupNamespace, indexedHash), db)
			if err != nil {
				t.Fatalf("indexed hash lookup should be populated for %s: %v", strategy, err)
			}
			if len(indexedHashIDs) == 0 {
				t.Fatalf("indexed hash lookup should contain at least one id for %s", strategy)
			}
		})
	}
}

func TestStoreHashStrategyPartitionIndexedParityAndListVisibility(t *testing.T) {
	ensureZstdReadyForTests(t)

	originalStrategy := cnst.StoreHashStrategy
	originalChunkSize := cnst.ChonkSize
	util.SetChonkSize(64)
	t.Cleanup(func() {
		cnst.StoreHashStrategy = originalStrategy
		cnst.ChonkSize = originalChunkSize
	})

	inputPath := filepath.Join(t.TempDir(), "sample.bin")
	chunk := []byte("partition-indexed-hocho-coverage")
	data := bytes.Repeat(chunk, 4096)
	if err := os.WriteFile(inputPath, data, 0o600); err != nil {
		t.Fatalf("write input file: %v", err)
	}

	handle, err := os.Open(inputPath)
	if err != nil {
		t.Fatalf("open input for hash: %v", err)
	}
	expectedEvidenceRaw, err := testutil.HashFile(handle, cnst.GetHashAlgo(true))
	closeErr := handle.Close()
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("close input handle: %v", closeErr)
	}

	key := []byte("01234567890123456789012345678901")

	dbPathSync := filepath.Join(t.TempDir(), "db-sync")
	cnst.StoreHashStrategy = cnst.SyncHashStrategy
	if err := StoreData(t.Context(), optsFor(dbPathSync, 64, key), inputPath, false, true, false, false); err != nil {
		t.Fatalf("StoreData sync: %v", err)
	}
	_, syncFID := collectEvidenceHashState(t, dbPathSync, key, expectedEvidenceRaw)

	dbPathHocho := filepath.Join(t.TempDir(), "db-hocho")
	cnst.StoreHashStrategy = cnst.HochoHashStrategy
	if err := StoreData(t.Context(), optsFor(dbPathHocho, 64, key), inputPath, false, true, false, false); err != nil {
		t.Fatalf("StoreData hocho: %v", err)
	}
	expectedHochoEvidence, err := computeExpectedHochoHashForTest(inputPath)
	if err != nil {
		t.Fatalf("computeSyncHochoHash: %v", err)
	}
	expectedHochoRaw, err := base64.StdEncoding.DecodeString(expectedHochoEvidence)
	if err != nil {
		t.Fatalf("decode expected hocho evidence hash: %v", err)
	}
	_, hochoFID := collectEvidenceHashState(t, dbPathHocho, key, expectedHochoRaw)

	syncPartitionHash, syncIndexedHash := attachSyntheticPartitionAndIndexed(t, dbPathSync, key, syncFID, inputPath, "sync", cnst.SyncHashStrategy)
	hochoPartitionHash, hochoIndexedHash := attachSyntheticPartitionAndIndexed(t, dbPathHocho, key, hochoFID, inputPath, "hocho", cnst.HochoHashStrategy)

	if syncPartitionHash != hochoPartitionHash {
		t.Fatalf("expected partition hash parity between sync and hocho, got sync=%q hocho=%q", syncPartitionHash, hochoPartitionHash)
	}
	if syncIndexedHash != hochoIndexedHash {
		t.Fatalf("expected indexed hash parity between sync and hocho, got sync=%q hocho=%q", syncIndexedHash, hochoIndexedHash)
	}

	assertListContainsSyntheticHierarchy(t, dbPathSync, key, syncPartitionHash, syncIndexedHash)
	assertListContainsSyntheticHierarchy(t, dbPathHocho, key, hochoPartitionHash, hochoIndexedHash)
}

func attachSyntheticPartitionAndIndexed(t *testing.T, dbPath string, key []byte, evidenceID []byte, inputPath string, tag string, strategy string) (string, string) {
	t.Helper()
	previousStrategy := cnst.StoreHashStrategy
	cnst.StoreHashStrategy = strategy
	defer func() { cnst.StoreHashStrategy = previousStrategy }()

	h, err := openTestDB(dbPath, key)
	if err != nil {
		t.Fatalf("ConnectDB: %v", err)
	}
	defer h.Close()
	db := h.DB

	evi, err := dbio.GetEvidenceFile(evidenceID, db)
	if err != nil {
		t.Fatalf("GetEvidenceFile: %v", err)
	}

	handle, err := os.Open(inputPath)
	if err != nil {
		t.Fatalf("open input for logical hash: %v", err)
	}
	defer handle.Close()

	partitionStart := int64(0)
	partitionSize := int64(512)
	indexedStart := int64(64)
	indexedSize := int64(256)

	partitionRaw, err := testutil.HashRange(handle, cnst.GetHashAlgo(true), partitionStart, partitionSize)
	if err != nil {
		t.Fatalf("partition logical hash: %v", err)
	}
	indexedRaw, err := testutil.HashRange(handle, cnst.GetHashAlgo(true), indexedStart, indexedSize)
	if err != nil {
		t.Fatalf("indexed logical hash: %v", err)
	}

	partitionHash := base64.StdEncoding.EncodeToString(partitionRaw)
	indexedHash := base64.StdEncoding.EncodeToString(indexedRaw)

	indexed := structs.NewIndexedFile("evidence://"+tag+"/inode.bin", indexedStart, indexedSize, cnst.UnknownEvidenceType, false, indexedHash)
	indexedBatch, err := util.InitBatch(db)
	if err != nil {
		t.Fatalf("InitBatch indexed: %v", err)
	}
	if err := dbio.SetIndexedFile(indexedRaw, indexed, indexedBatch); err != nil {
		t.Fatalf("SetFile indexed: %v", err)
	}
	if err := indexedBatch.Flush(); err != nil {
		t.Fatalf("Flush indexed batch: %v", err)
	}
	indexedKey := dbio.CanonicalIndexedKey(indexedRaw)
	indexedLookupKey := util.AppendToBytesSlice(cnst.IdxFileHashLookupNamespace, indexedHash)
	if err := dbio.AppendHashLookupUUIDByKey(db, indexedLookupKey, indexedKey); err != nil {
		t.Fatalf("set indexed hash lookup: %v", err)
	}

	partition := structs.NewPartitionFile("evidence://"+tag+"/partition0", partitionStart, partitionSize, map[string]structs.InternalOffset{
		indexedHash: {
			Start: indexedStart,
			End:   indexedStart + indexedSize,
		},
	}, partitionHash)
	partitionKey := util.AppendToBytesSlice(cnst.PartiFileNamespace, partitionRaw)
	if err := dbio.SetFile(partitionKey, partition, db); err != nil {
		t.Fatalf("SetFile partition: %v", err)
	}
	partitionLookupKey := util.AppendToBytesSlice(cnst.PartiFileHashLookupNamespace, partitionHash)
	if err := dbio.AppendHashLookupUUIDByKey(db, partitionLookupKey, partitionKey); err != nil {
		t.Fatalf("set partition hash lookup: %v", err)
	}

	if evi.InternalObjects == nil {
		evi.InternalObjects = make(map[string]structs.InternalOffset)
	}
	evi.InternalObjects[partitionHash] = structs.InternalOffset{Start: partitionStart, End: partitionStart + partitionSize}
	if err := dbio.SetFile(evidenceID, evi, db); err != nil {
		t.Fatalf("SetFile evidence: %v", err)
	}

	if _, err := dbio.GetPartitionFile(partitionRaw, db); err != nil {
		t.Fatalf("GetPartitionFile: %v", err)
	}
	if _, err := dbio.GetIndexedFile(indexedRaw, db); err != nil {
		t.Fatalf("GetIndexedFile: %v", err)
	}

	return partitionHash, indexedHash
}

func assertListContainsSyntheticHierarchy(t *testing.T, dbPath string, key []byte, expectedPartitionHash string, expectedIndexedHash string) {
	t.Helper()

	h, err := openTestDB(dbPath, key)
	if err != nil {
		t.Fatalf("ConnectDB: %v", err)
	}
	defer h.Close()
	db := h.DB

	var out bytes.Buffer
	console.Configure(&out, io.Discard)
	listErr := store.List(db, "")
	console.Reset()
	if listErr != nil {
		t.Fatalf("store.List: %v", listErr)
	}
	output := out.Bytes()

	var decoded map[string]interface{}
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("decode list output: %v\noutput: %s", err, string(output))
	}

	evidence, ok := decoded["evidence"].([]interface{})
	if !ok || len(evidence) == 0 {
		t.Fatalf("expected non-empty evidence list: %s", string(output))
	}

	foundPartition := false
	foundIndexed := false
	for _, e := range evidence {
		eviMap, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		partitions, ok := eviMap["partitions"].([]interface{})
		if !ok {
			continue
		}
		for _, p := range partitions {
			pmap, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			if pmap["hash"] == expectedPartitionHash {
				foundPartition = true
			}
			indexed, _ := pmap["indexedFiles"].([]interface{})
			for _, i := range indexed {
				imap, ok := i.(map[string]interface{})
				if ok && imap["hash"] == expectedIndexedHash {
					foundIndexed = true
				}
			}
		}
	}

	if !foundPartition {
		t.Fatalf("partition hash %q not present in list output: %s", expectedPartitionHash, string(output))
	}
	if !foundIndexed {
		t.Fatalf("indexed hash %q not present in list output: %s", expectedIndexedHash, string(output))
	}
}
