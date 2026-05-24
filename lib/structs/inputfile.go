package structs

import (
	"encoding/base64"
	"os"

	"github.com/dgraph-io/badger/v4"
	"github.com/edsrzf/mmap-go"
	"github.com/google/uuid"
)

type InputFile struct {
	fileHandle      *os.File
	mappedFile      mmap.MMap
	size            int64
	id              []byte
	namespace       string
	fileHash        []byte
	name            string
	startIndex      int64
	db              *badger.DB
	batch           *badger.WriteBatch
	internalObjects map[string]InternalOffset
}

// NewInputFile creates a new InputFile with UUIDv7 ID as the default behavior.
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
	uuidv7, err := uuid.NewV7()
	if err != nil {
		panic(err)
	}
	infile.id = uuidv7[:]
	infile.namespace = namespace
	infile.fileHash = inFileHash
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
func (i InputFile) GetMappedFile() mmap.MMap {
	return i.mappedFile
}
func (i InputFile) GetID() []byte {
	return i.id
}

// GetFileID returns this file object's UUIDv7 identifier.
func (i InputFile) GetFileID() []byte {
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

// GetHash returns the file ID for compatibility with existing callsites.
func (i InputFile) GetHash() []byte {
	return i.id
}

func (i InputFile) GetFileHash() []byte {
	return i.fileHash
}

// GetEncodedHash returns base64(file_id).
func (i InputFile) GetEncodedHash() ([]byte, error) {
	hash := i.GetHash()
	return []byte(base64.StdEncoding.EncodeToString(hash)), nil
}
func (i InputFile) GetInternalObjects() map[string]InternalOffset {
	return i.internalObjects
}

func (i InputFile) GetNamespace() []byte {
	return []byte(i.namespace)
}

func (i *InputFile) UpdateInternalObjects(start, size int64, objectHash []byte) {
	objHashStr := base64.StdEncoding.EncodeToString(objectHash)
	end := (start + size) - 1
	i.internalObjects[objHashStr] = InternalOffset{start, end}
}
