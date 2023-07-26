package util

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"indicer/lib/cnst"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cheggaaa/pb/v3"
	"github.com/zeebo/blake3"
)

var globalHasherPool = sync.Pool{
	New: func() interface{} {
		return blake3.New()
	},
}

func GetDBPath() (string, error) {
	const dbdir = ".dues"
	path, err := os.UserHomeDir()
	if err != nil {
		path, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}

	fullpath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	dbpath := filepath.Join(fullpath, dbdir)
	err = os.MkdirAll(dbpath, os.ModeDir)

	return dbpath, err
}

func EnsureBlobPath(dbpath string) error {
	blobpath := filepath.Join(dbpath, cnst.BLOBSDIR)
	_, err := os.Stat(blobpath)
	if err != nil {
		return err
	}
	if os.IsExist(err) {
		return nil
	}
	return os.Mkdir(blobpath, os.ModeDir)
}

func SetChonkSize(chonkSize int) {
	cnst.ChonkSize = int64(chonkSize) * cnst.KB
}

func GetFileHash(fileHandle *os.File) ([]byte, error) {
	info, err := os.Stat(fileHandle.Name())
	if err != nil {
		return nil, err
	}

	hash, err := getHash(fileHandle, info.Size())
	if err != nil {
		return nil, err
	}

	_, err = fileHandle.Seek(0, io.SeekStart)
	return hash, err
}

func GetLogicalFileHash(fileHandle *os.File, start, size int64) ([]byte, error) {
	_, err := fileHandle.Seek(start, io.SeekStart)
	if err != nil {
		return nil, err
	}

	hash, err := getHash(fileHandle, size)
	if err != nil {
		return nil, err
	}

	_, err = fileHandle.Seek(0, io.SeekStart)
	return hash, err
}

func getHash(fileHandle *os.File, size int64) ([]byte, error) {
	hasher := globalHasherPool.Get().(*blake3.Hasher)
	defer globalHasherPool.Put(hasher)

	fmt.Println("Generating BLAKE3 hash ....")
	startTime := time.Now()

	bar := pb.Full.Start64(size)
	barReader := bar.NewProxyReader(fileHandle)

	bufferSize := 4 * cnst.MB
	buffer := make([]byte, bufferSize)
	for size > 0 {
		chunkSize := int64(bufferSize)
		if size < int64(bufferSize) {
			chunkSize = size
		}

		_, err := barReader.Read(buffer[:chunkSize])
		if err != nil {
			return nil, err
		}

		_, err = hasher.Write(buffer[:chunkSize])
		if err != nil {
			return nil, err
		}

		size -= chunkSize
	}
	hash := hasher.Sum(nil)

	bar.Finish()
	fmt.Printf("Operation completed in: %s\n\n", time.Since(startTime))
	return hash, nil
}

func GetChonkHash(data []byte) ([]byte, error) {
	hasher := globalHasherPool.Get().(*blake3.Hasher)
	defer globalHasherPool.Put(hasher)
	if _, err := hasher.Write(data); err != nil {
		return nil, err
	}
	return hasher.Sum(nil), nil
}

func IsLogicalFile(inid []byte) bool {
	return bytes.HasPrefix(inid, []byte(cnst.PartiFileNamespace)) ||
		bytes.HasPrefix(inid, []byte(cnst.IdxFileNamespace))
}

func AppendToBytesSlice(args ...interface{}) []byte {
	var buffer bytes.Buffer

	for _, arg := range args {
		switch value := arg.(type) {
		case []byte:
			buffer.Write(value)
		case string:
			buffer.WriteString(value)
		case int64:
			buffer.Write(strconv.AppendInt(nil, value, 10))
		case int:
			buffer.Write(strconv.AppendInt(nil, int64(value), 10))
		default:
			buffer.WriteString("Unsupported Type")
		}
	}

	return buffer.Bytes()
}

func HashPassword(password string) []byte {
	hash := sha256.Sum256([]byte(password))
	return hash[:]
}

func PartialMatchConfidence(s1, s2 []byte) float32 {
	minLength := len(s1)
	if len(s2) < minLength {
		minLength = len(s2)
	}

	similarCount := 0
	for i := 0; i < minLength; i++ {
		if s1[i] == s2[i] {
			similarCount++
		}
	}

	return float32(similarCount) / float32(minLength)
}

func GetDBStartOffset(startIndex int64) int64 {
	if startIndex == 0 {
		return 0
	}
	return (startIndex / cnst.ChonkSize) * cnst.ChonkSize
}

func GetDBEndOffset(endIndex int64) int64 {
	if endIndex == 0 {
		return 0
	}
	return (endIndex-1)/cnst.ChonkSize*cnst.ChonkSize + cnst.ChonkSize
}

func GetEvidenceFileHash(fname string) ([]byte, error) {
	eviFileHashString := strings.Split(fname, cnst.DataSeperator)[0]
	eviFileHash, err := base64.StdEncoding.DecodeString(eviFileHashString)
	if err != nil {
		return nil, err
	}
	return eviFileHash, err
}
func GetEvidenceFileID(eviFileHash []byte) []byte {
	return append([]byte(cnst.EviFileNamespace), eviFileHash...)
}

func GetRandomName(length int) string {
	randomBytes := make([]byte, 32)
	_, err := rand.Read(randomBytes)
	if err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(randomBytes)[:length]
}
