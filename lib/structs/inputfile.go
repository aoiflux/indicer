package structs

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/util"
	"os"
	"strings"

	"github.com/dgraph-io/badger/v3"
	"github.com/edsrzf/mmap-go"
)

type InputFile struct {
	mappedFile      mmap.MMap
	fileHandle      *os.File
	size            int64
	id              []byte
	name            string
	startIndex      int64
	db              *badger.DB
	internalObjects []string
}

func NewInputFile(
	db *badger.DB,
	fileHandle *os.File,
	mappedFile mmap.MMap,
	name, namespace string,
	inFileHash []byte,
	size, startIndex int64,
) InputFile {
	var infile InputFile
	infile.fileHandle = fileHandle
	infile.mappedFile = mappedFile
	infile.id = util.AppendToBytesSlice(namespace, inFileHash)
	infile.name = name
	infile.db = db
	infile.size = size
	infile.startIndex = startIndex
	infile.internalObjects = make([]string, 0)

	return infile
}
func (i InputFile) GetMappedFile() mmap.MMap {
	return i.mappedFile
}
func (i InputFile) GetHandle() *os.File {
	return i.fileHandle
}
func (i InputFile) GetID() []byte {
	return i.id
}
func (i InputFile) GetName() string {
	return i.name
}
func (i InputFile) GetStartIndex() int64 {
	return i.startIndex
}
func (i *InputFile) GetEndIndex() int64 {
	return i.startIndex + i.size
}
func (i InputFile) GetDB() *badger.DB {
	return i.db
}
func (i InputFile) GetSize() int64 {
	return i.size
}
func (i InputFile) GetHash() []byte {
	return bytes.Split(i.id, []byte(cnst.NamespaceSeperator))[1]
}
func (i InputFile) GetEncodedHash() ([]byte, error) {
	hash := i.GetHash()
	return []byte(base64.StdEncoding.EncodeToString(hash)), nil
}
func (i InputFile) GetInternalObjects() []string {
	return i.internalObjects
}
func (i InputFile) GetEviFileHash() []byte {
	if strings.HasPrefix(i.name, cnst.EviFileNamespace) {
		return i.GetHash()
	}
	ehashString := strings.Split(i.name, cnst.DataSeperator)[0]
	return []byte(ehashString)
}
func (i InputFile) GetNamespace() []byte {
	fileType := bytes.Split(i.id, []byte(cnst.NamespaceSeperator))[0]
	return append(fileType, []byte(cnst.NamespaceSeperator)...)
}
func (i *InputFile) UpdateInternalObjects(start, size int64, objectHash []byte) {
	objHashStr := base64.StdEncoding.EncodeToString(objectHash)
	for _, item := range i.internalObjects {
		internalObjectHash := strings.Split(item, cnst.DataSeperator)[0]
		if internalObjectHash == objHashStr {
			return
		}
	}
	dbstart := util.GetDBStartOffset(start)
	objHashStr = fmt.Sprintf("%s%s%d%s%d", objHashStr, cnst.DataSeperator, dbstart, cnst.RangeSeperator, start+size)
	i.internalObjects = append(i.internalObjects, objHashStr)
}
