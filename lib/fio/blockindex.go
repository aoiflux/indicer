package fio

// Legacy hierarchical block index, read only.
//
// Hierarchical mode (--hierarchical) kept container chunk locations in block
// files, blob/blocks/block_<first hash byte>.bidx, instead of in badger. D9
// retired it: new chunk locations always go to C|||, and nothing writes block
// files any more. Databases that have them stay readable through
// LookupBlockIndex, and `dues repair --upgrade` moves their records into
// C||| (ReadBlockIndex).
//
// A v2 block file is a 9-byte header ("BIDX", version 2, record count as a
// little-endian uint32), the records sorted by hash (64-byte zero-padded
// hash, offset int64, size int64, path offset uint32, path length uint16; 86
// bytes each), then the string table holding the container paths.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/util"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	blockIndexDir   = "blocks"
	blockIndexExt   = ".bidx"
	blockMagic      = "BIDX"
	blockVersion    = 2
	blockHeaderSize = 9
	blockRecordSize = 86
	blockHashSize   = 64
)

// ErrChunkNotIndexed is a block-index lookup for a chunk the index does not
// hold, as opposed to an index that could not be read.
var ErrChunkNotIndexed = errors.New("chunk not in block index")

// ErrBlockIndexCorrupt reports a block file that fails its format checks.
var ErrBlockIndexCorrupt = errors.New("block index is corrupt")

// BlockIndexRecord is one chunk location in a legacy block index.
type BlockIndexRecord struct {
	Hash         []byte
	Path         string
	Offset, Size int64
}

// LookupBlockIndex returns the container, offset and size the block index of
// the database at dbpath records for the chunk key ckey. It returns an error
// wrapping ErrChunkNotIndexed when the index does not hold the chunk, and
// any other error when the index cannot be read or is damaged.
func LookupBlockIndex(dbpath string, ckey []byte) (string, int64, int64, error) {
	hash := bytes.TrimPrefix(ckey, []byte(cnst.ChonkNamespace))
	if len(hash) == 0 || len(hash) > blockHashSize {
		return "", 0, 0, fmt.Errorf("%w: chunk hash of %d bytes", ErrChunkNotIndexed, len(hash))
	}
	path := blockFilePath(dbpath, hash)
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", 0, 0, fmt.Errorf("%w: no block file %s", ErrChunkNotIndexed, filepath.Base(path))
	}
	if err != nil {
		return "", 0, 0, err
	}
	defer f.Close()
	b, err := openBlockFile(f, path)
	if err != nil {
		return "", 0, 0, err
	}

	var target [blockHashSize]byte
	copy(target[:], hash)
	rec := make([]byte, blockRecordSize)
	lo, hi := uint64(0), b.count
	for lo < hi {
		mid := lo + (hi-lo)/2
		if err := b.record(mid, rec); err != nil {
			return "", 0, 0, err
		}
		switch cmp := bytes.Compare(rec[:blockHashSize], target[:]); {
		case cmp == 0:
			r, err := b.decode(rec)
			if err != nil {
				return "", 0, 0, err
			}
			return r.Path, r.Offset, r.Size, nil
		case cmp < 0:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return "", 0, 0, fmt.Errorf("%w: not in %s", ErrChunkNotIndexed, filepath.Base(path))
}

// BlockIndexFiles lists the block files of the database at dbpath.
func BlockIndexFiles(dbpath string) ([]string, error) {
	return filepath.Glob(filepath.Join(util.BlobPath(dbpath), blockIndexDir, "block_*"+blockIndexExt))
}

// ReadBlockFile calls fn for every record of the block file at path, in hash
// order. hashLen is the database's chunk hash length; the zero padding past it
// is dropped.
func ReadBlockFile(path string, hashLen int, fn func(BlockIndexRecord) error) error {
	if hashLen <= 0 || hashLen > blockHashSize {
		return fmt.Errorf("chunk hash length %d", hashLen)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := openBlockFile(f, path)
	if err != nil {
		return err
	}
	rec := make([]byte, blockRecordSize)
	for i := range b.count {
		if err := b.record(i, rec); err != nil {
			return err
		}
		r, err := b.decode(rec)
		if err != nil {
			return err
		}
		r.Hash = bytes.Clone(rec[:hashLen])
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

// blockFile is an open, header-checked block file.
type blockFile struct {
	f          *os.File
	path       string
	count      uint64
	tableStart uint64
	size       uint64
}

// openBlockFile checks the header of f and that the file is large enough for
// the records it claims, before anything is read or allocated from them.
func openBlockFile(f *os.File, path string) (*blockFile, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	b := &blockFile{f: f, path: path, size: uint64(fi.Size())}
	header := make([]byte, blockHeaderSize)
	if b.size < blockHeaderSize {
		return nil, b.corrupt("shorter than its header")
	}
	if _, err := f.ReadAt(header, 0); err != nil {
		return nil, err
	}
	if string(header[:4]) != blockMagic {
		return nil, b.corrupt("unknown format")
	}
	if header[4] != blockVersion {
		return nil, b.corrupt(fmt.Sprintf("unsupported version %d", header[4]))
	}
	b.count = uint64(binary.LittleEndian.Uint32(header[5:9]))
	b.tableStart = blockHeaderSize + b.count*blockRecordSize
	if b.tableStart > b.size {
		return nil, b.corrupt(fmt.Sprintf("%d records do not fit in %d bytes", b.count, b.size))
	}
	return b, nil
}

func (b *blockFile) corrupt(what string) error {
	return fmt.Errorf("%w: %s: %s", ErrBlockIndexCorrupt, b.path, what)
}

// record reads record i into rec.
func (b *blockFile) record(i uint64, rec []byte) error {
	_, err := b.f.ReadAt(rec, int64(blockHeaderSize+i*blockRecordSize))
	return err
}

// decode returns the location in rec, reading its path from the string table.
func (b *blockFile) decode(rec []byte) (BlockIndexRecord, error) {
	offset := int64(binary.LittleEndian.Uint64(rec[64:72]))
	size := int64(binary.LittleEndian.Uint64(rec[72:80]))
	pathOff := uint64(binary.LittleEndian.Uint32(rec[80:84]))
	pathLen := uint64(binary.LittleEndian.Uint16(rec[84:86]))
	if offset < 0 || size <= 0 {
		return BlockIndexRecord{}, b.corrupt(fmt.Sprintf("record with offset %d and size %d", offset, size))
	}
	if pathLen == 0 || b.tableStart+pathOff+pathLen > b.size {
		return BlockIndexRecord{}, b.corrupt("container path outside the string table")
	}
	path := make([]byte, pathLen)
	if _, err := b.f.ReadAt(path, int64(b.tableStart+pathOff)); err != nil {
		return BlockIndexRecord{}, err
	}
	return BlockIndexRecord{Path: string(path), Offset: offset, Size: size}, nil
}

func blockFilePath(dbpath string, hash []byte) string {
	return filepath.Join(util.BlobPath(dbpath), blockIndexDir, fmt.Sprintf("block_%x%s", hash[:1], blockIndexExt))
}
