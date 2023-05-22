package util

import (
	"bytes"
	"fmt"
	"indicer/lib/constant"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cheggaaa/pb/v3"
	"github.com/diskfs/go-diskfs/partition/mbr"
	"github.com/zeebo/blake3"
)

func GetDBPath() (string, error) {
	path := os.Args[2]
	finfo, err := os.Stat(path)

	if os.IsNotExist(err) {
		err = os.MkdirAll(path, os.ModeDir)
		return path, err
	}

	if !finfo.IsDir() {
		path = filepath.Dir(path)
	}

	abspath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	return abspath, err
}

func SetChonkSize(chonkSizeString string) {
	newChonkSize, err := strconv.ParseInt(chonkSizeString, 10, 64)
	if err != nil {
		return
	}
	constant.ChonkSize = newChonkSize
}

func GetFileHash(fileHandle *os.File) ([]byte, error) {
	info, err := os.Stat(fileHandle.Name())
	if err != nil {
		return nil, err
	}

	fmt.Println("Generating BLAKE3 hash for file: ", fileHandle.Name())
	start := time.Now()
	fileHandle.Seek(0, io.SeekStart)

	hasher := blake3.New()
	bar := pb.Full.Start64(info.Size())
	barReader := bar.NewProxyReader(fileHandle)

	_, err = io.Copy(hasher, barReader)
	if err != nil {
		return nil, err
	}
	hash := hasher.Sum(nil)

	bar.Finish()
	fmt.Printf("Operation completed in: %s\n\n", time.Since(start))
	_, err = fileHandle.Seek(0, io.SeekStart)
	return hash, err
}

func GetLogicalFileHash(fhandle *os.File, start, size int64) ([]byte, error) {
	_, err := fhandle.Seek(start, io.SeekStart)
	if err != nil {
		return nil, err
	}

	hasher := blake3.New()
	_, err = io.CopyN(hasher, fhandle, size)
	if err != nil {
		return nil, err
	}
	hash := hasher.Sum(nil)

	_, err = fhandle.Seek(0, io.SeekStart)
	return hash, err
}

func GetChonkHash(data []byte) ([]byte, error) {
	hasher := blake3.New()
	reader := bytes.NewReader(data)
	_, err := io.Copy(hasher, reader)
	if err != nil {
		return nil, err
	}
	return hasher.Sum(nil), nil
}

func IsLogicalFile(inid []byte) bool {
	return bytes.HasPrefix(inid, []byte(constant.PartitionFileNamespace)) ||
		bytes.HasPrefix(inid, []byte(constant.IndexedFileNamespace))
}

// IsSupported checks if detected file system is supported or not
// Both exFAT & NTFS same the share partition type number ie 0x07
func IsSupported(ptype mbr.Type) bool {
	return ptype == mbr.NTFS
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
			buffer.WriteString(strconv.FormatInt(value, 10))
		default:
			buffer.WriteString("Unsupported Type")
		}
	}

	return buffer.Bytes()
}
