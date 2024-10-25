package structs

import (
	"bytes"
	"encoding/base64"
	"indicer/lib/cnst"
	"indicer/lib/util"
	"os"
	"strings"

	"github.com/dgraph-io/badger/v4"
)

type InputFile struct {
	fileHandle      *os.File
	size            int64
	id              []byte
	name            string
	startIndex      int64
	db              *badger.DB
	batch           *badger.WriteBatch
	internalObjects map[string]InternalOffset
}

func NewInputFile(
	db *badger.DB,
	fileHandle *os.File,
	name, namespace string,
	inFileHash []byte,
	size, startIndex int64,
) InputFile {
	var infile InputFile

	infile.fileHandle = fileHandle
	infile.id = util.AppendToBytesSlice(namespace, inFileHash)
	infile.name = name
	infile.db = db
	infile.size = size
	infile.startIndex = startIndex
	infile.internalObjects = make(map[string]InternalOffset, 0)
	infile.batch = nil

	return infile
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
func (i InputFile) GetInternalObjects() map[string]InternalOffset {
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
	end := (start + size) - 1
	i.internalObjects[objHashStr] = InternalOffset{start, end}
}

func (i *InputFile) UpdateInputFile(name, namespace string, hash []byte, size, start int64) {
	i.name = name
	i.id = util.AppendToBytesSlice(namespace, hash)
	i.size = size
	i.startIndex = start
}
