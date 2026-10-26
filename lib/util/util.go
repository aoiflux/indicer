package util

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha3"
	"encoding/base64"
	"fmt"
	"hash"
	"hash/fnv"
	"indicer/internal/console"
	"indicer/lib/cnst"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cheggaaa/pb/v3"
	"github.com/dgraph-io/badger/v4"
)

func EnsureBlobPath(dbpath string) error {
	blobpath := BlobPath(dbpath)
	_, err := os.Stat(blobpath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if os.IsExist(err) {
		return nil
	}
	return os.MkdirAll(blobpath, cnst.DirPerm)
}

func ResolveDataRoot(path string) string {
	clean := filepath.Clean(path)
	if filepath.Base(clean) == cnst.KVDBDIR {
		return filepath.Dir(clean)
	}
	return clean
}

func KVDBPath(path string) string {
	return filepath.Join(ResolveDataRoot(path), cnst.KVDBDIR)
}

func BlobPath(path string) string {
	return filepath.Join(ResolveDataRoot(path), cnst.BLOBSDIR)
}

func GraphPath(path string) string {
	return filepath.Join(ResolveDataRoot(path), cnst.GRAPHDIR)
}

func FTSPath(path string) string {
	return filepath.Join(ResolveDataRoot(path), cnst.FTSDIR)
}

func SetChonkSize(chonkSize int) {
	cnst.ChonkSize = int64(chonkSize) * cnst.KB
}

func GetFileHash(fileHandle *os.File, hasher hash.Hash) ([]byte, error) {
	info, err := os.Stat(fileHandle.Name())
	if err != nil {
		return nil, err
	}

	_, err = fileHandle.Seek(0, io.SeekStart)
	if err != nil {
		return nil, err
	}

	hash, err := getHash(fileHandle, hasher, info.Size(), true)
	if err != nil {
		return nil, err
	}

	_, err = fileHandle.Seek(0, io.SeekStart)
	return hash, err
}

func GetLogicalFileHash(fileHandle *os.File, hasher hash.Hash, start, size int64, showBar bool) ([]byte, error) {
	_, err := fileHandle.Seek(start, io.SeekStart)
	if err != nil {
		return nil, err
	}

	hash, err := getHash(fileHandle, hasher, size, showBar)
	if err != nil {
		return nil, err
	}

	_, err = fileHandle.Seek(0, io.SeekStart)
	return hash, err
}

func getHash(fileHandle *os.File, hasher hash.Hash, size int64, showBar bool) ([]byte, error) {
	var startTime time.Time
	if showBar {
		console.Printf("Generating %s hash ....\n", cnst.HASHALGO)
		startTime = time.Now()
	}

	bar := pb.New64(100)
	if showBar {
		bar = pb.New64(size).SetTemplate(pb.Full).SetWriter(console.Progress()).Start()
	}
	barReader := bar.NewProxyReader(fileHandle)

	_, err := io.CopyN(hasher, barReader, size)
	if err != nil {
		return nil, err
	}
	hash := hasher.Sum(nil)

	if showBar {
		bar.Finish()
		console.Printf("Operation completed in: %s\n\n", time.Since(startTime))
	}
	return hash, nil
}

func GetChonkHash(data []byte, hasher hash.Hash) ([]byte, error) {
	if _, err := hasher.Write(data); err != nil {
		return nil, err
	}
	return hasher.Sum(nil), nil
}

func AppendToBytesSlice(args ...interface{}) []byte {
	const unsupported = "Unsupported Type"

	total := 0
	for _, arg := range args {
		switch value := arg.(type) {
		case []byte:
			total += len(value)
		case string:
			total += len(value)
		case int64:
			total += len(strconv.FormatInt(value, 10))
		case int:
			total += len(strconv.FormatInt(int64(value), 10))
		default:
			total += len(unsupported)
		}
	}

	out := make([]byte, 0, total)
	for _, arg := range args {
		switch value := arg.(type) {
		case []byte:
			out = append(out, value...)
		case string:
			out = append(out, value...)
		case int64:
			out = strconv.AppendInt(out, value, 10)
		case int:
			out = strconv.AppendInt(out, int64(value), 10)
		default:
			out = append(out, unsupported...)
		}
	}

	return out
}

func HashPassword(password string) ([]byte, error) {
	hasher := sha3.New256()
	_, err := hasher.Write([]byte(password))
	if err != nil {
		return nil, err
	}
	hash := hasher.Sum(nil)
	return hash[:], nil
}

func ChunkSimHash64(data []byte) uint64 {
	if len(data) == 0 {
		return 0
	}

	window := 4
	if len(data) < window {
		window = len(data)
	}

	weights := [64]int{}
	hasher := fnv.New64a()
	for i := 0; i+window <= len(data); i++ {
		hasher.Reset()
		_, _ = hasher.Write(data[i : i+window])
		h := hasher.Sum64()

		for bit := 0; bit < 64; bit++ {
			if (h & (uint64(1) << bit)) != 0 {
				weights[bit]++
			} else {
				weights[bit]--
			}
		}
	}

	var sig uint64
	for bit := 0; bit < 64; bit++ {
		if weights[bit] >= 0 {
			sig |= uint64(1) << bit
		}
	}

	return sig
}

func GetDBStartOffset(startIndex int64) int64 {
	if startIndex == 0 {
		return 0
	}
	return (startIndex / cnst.ChonkSize) * cnst.ChonkSize
}

// rawEvidenceIDLen is the length of an evidence ID, a UUIDv7.
const rawEvidenceIDLen = 16

// GetEvidenceFileHash returns the evidence ID a partition or indexed file
// name starts with. Partition names carry it base64-encoded; the parser names
// indexed files by the raw 16 bytes (ING-19), which may contain the separator
// itself. The raw form is recognised by the separator right after 16 bytes,
// where a base64 prefix cannot have one, so it is checked first: a raw ID of
// base64-alphabet bytes would otherwise decode to the wrong ID.
func GetEvidenceFileHash(fname string) ([]byte, error) {
	sep := cnst.DataSeperator
	if len(fname) >= rawEvidenceIDLen+len(sep) && fname[rawEvidenceIDLen:rawEvidenceIDLen+len(sep)] == sep {
		return []byte(fname[:rawEvidenceIDLen]), nil
	}
	prefix, _, _ := strings.Cut(fname, sep)
	eviFileHash, err := base64.StdEncoding.DecodeString(prefix)
	if err != nil {
		return nil, err
	}
	if len(eviFileHash) == 0 {
		return nil, fmt.Errorf("name %q does not start with an evidence ID", fname)
	}
	return eviFileHash, nil
}
func SealAES(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	return append(nonce, ciphertext...), nil
}
func UnsealAES(key, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) > nonceSize {
		plaintext, err := gcm.Open(nil, ciphertext[:nonceSize], ciphertext[nonceSize:], nil)
		if err == nil {
			return plaintext, nil
		}
	}

	legacyNonce := sha256.Sum256(key)
	plaintext, err := gcm.Open(nil, legacyNonce[:nonceSize], ciphertext, nil)
	if err != nil {
		return nil, err
	}

	return plaintext, nil
}

// InitBatch returns a write batch whose pending transactions are bounded by
// cnst.GetMaxBatchCount. The batch is created only once that succeeds, so an
// error leaks nothing (CFG-10).
func InitBatch(db *badger.DB) (*badger.WriteBatch, error) {
	count, err := cnst.GetMaxBatchCount()
	if err != nil {
		return nil, err
	}
	batch := db.NewWriteBatch()
	batch.SetMaxPendingTxns(count)
	return batch, nil
}
