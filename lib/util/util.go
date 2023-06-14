package util

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"indicer/lib/cnst"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cheggaaa/pb/v3"
	"github.com/diskfs/go-diskfs/partition/mbr"
	"github.com/zeebo/blake3"
	"golang.org/x/term"
)

func GetDBPath() (string, error) {
	const dbdir = ".indicer"

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

func SetChonkSize(chonkSizeString string) {
	newChonkSize, err := strconv.ParseInt(chonkSizeString, 10, 64)
	if err != nil {
		return
	}
	cnst.ChonkSize = newChonkSize
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
	return bytes.HasPrefix(inid, []byte(cnst.PartiFileNamespace)) ||
		bytes.HasPrefix(inid, []byte(cnst.IdxFileNamespace))
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
		case int:
			buffer.WriteString(strconv.FormatInt(int64(value), 10))
		default:
			buffer.WriteString("Unsupported Type")
		}
	}

	return buffer.Bytes()
}

func GetPassword() []byte {
	fmt.Print("Password: ")
	password, err := readPassword()
	if err != nil {
		fmt.Println("error reading input. using empty password")
		return []byte{}
	}
	hash := sha256.Sum256(password)
	return hash[:]
}

func readPassword() ([]byte, error) {
	password, err := term.ReadPassword(int(os.Stdin.Fd()))
	if err != nil {
		return nil, err
	}
	fmt.Println()
	return password, nil
}

func ByteSimilarityCount(s1, s2 []byte) float64 {
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

	return float64(similarCount) / float64(minLength)
}

func GetDBStartOffset(startIndex int64) int64 {
	if startIndex == 0 {
		return 0
	}

	ans := float64(startIndex) / float64(cnst.ChonkSize)
	ans = math.Floor(ans)

	offset := int64(ans) * cnst.ChonkSize
	return offset
}

func GetEvidenceFileHash(fname string) ([]byte, error) {
	eviFileHashString := strings.Split(fname, cnst.FilePathSeperator)[0]
	eviFileHash, err := base64.StdEncoding.DecodeString(eviFileHashString)
	if err != nil {
		return nil, err
	}
	return eviFileHash, err
}
func GetEvidenceFileID(eviFileHash []byte) []byte {
	return append([]byte(cnst.EviFileNamespace), eviFileHash...)
}
