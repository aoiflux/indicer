package fio

import (
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"indicer/lib/blobcodec"
	"indicer/lib/cnst"
	"indicer/lib/fsx"
	"indicer/lib/util"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// WriteChonk stores data, a chunk's encoded bytes, as the blob for the chunk
// key ckey in the blob directory beside the database at dbpath, and returns
// the blob's path. The blob is named by ckey's hash under newHash, the
// database's chunk hash.
//
// The blob is synced before WriteChonk returns (STO-4). Ingest writes the
// chunk's C||| metadata only after that, and nothing trusts a blob without
// metadata recording its size, so a blob torn by a crash is never read.
//
// A new blob is created in place, which avoids a rename per chunk. An
// existing blob is replaced through a temporary file and an atomic rename:
// ingest calls this only for a chunk whose blob is missing or not the size
// its metadata records, so an existing file is a torn or truncated blob, and
// the metadata that may still point at it must never see a short file.
func WriteChonk(dbpath string, data, ckey []byte, newHash func() hash.Hash) ([]byte, error) {
	ckhash, err := util.GetChonkHash(ckey, newHash())
	if err != nil {
		return nil, err
	}
	cfname := base64.RawURLEncoding.EncodeToString(ckhash) + cnst.BLOBEXT
	cfpath := filepath.Join(util.BlobPath(dbpath), cfname)
	if err := blobWrites.do(cfpath, func() error {
		err := writeNewFile(cfpath, data, cnst.BlobPerm)
		if errors.Is(err, fs.ErrExist) {
			err = replaceFile(cfpath, data, cnst.BlobPerm)
		}
		return err
	}); err != nil {
		return nil, err
	}
	return []byte(cfpath), nil
}

// blobWrites makes writers of one blob in this process take turns: ingests
// running at once (two ingests of the same content, an ingest's index pass
// storing decoded content beside its chunk pass) write the same new chunk at
// the same time. A second writer used to find the first one's half-written
// file, take it for a torn blob and rename a replacement over it while it was
// still open, which Windows refuses. Now it waits, and uses the first one's
// blob when that succeeded. Writers in other processes are excluded by the
// data root's lock.
var blobWrites = newSingleFlight()

type singleFlight struct {
	mu       sync.Mutex
	inFlight map[string]*flight
}

type flight struct {
	done chan struct{}
	err  error
}

func newSingleFlight() *singleFlight { return &singleFlight{inFlight: map[string]*flight{}} }

// do runs write for key unless another call for key is running, in which
// case it waits for that one and returns nil if it succeeded. If it failed,
// do tries again itself.
func (s *singleFlight) do(key string, write func() error) error {
	for {
		s.mu.Lock()
		if f, ok := s.inFlight[key]; ok {
			s.mu.Unlock()
			<-f.done
			if f.err == nil {
				return nil
			}
			continue
		}
		f := &flight{done: make(chan struct{})}
		s.inFlight[key] = f
		s.mu.Unlock()

		f.err = write()
		s.mu.Lock()
		delete(s.inFlight, key)
		s.mu.Unlock()
		close(f.done)
		return f.err
	}
}

// The two ways a blob is written; tests replace them.
var (
	writeNewFile = fsx.WriteNewFile
	replaceFile  = fsx.WriteFileAtomic
)

// ReadChonk returns the stored (still encoded) bytes of a chunk blob;
// lib/blobcodec decodes them.
func ReadChonk(cfpath string) ([]byte, error) {
	return os.ReadFile(cfpath)
}

// ReadChonkLimit is ReadChonk for a blob whose path comes from a record that
// may be crafted: it reads only a regular file of at most limit bytes, so a
// link or an oversized file is refused before it is read. Either is
// blobcodec.ErrCorrupt.
func ReadChonkLimit(cfpath string, limit int64) ([]byte, error) {
	named, err := os.Lstat(cfpath)
	if err != nil {
		return nil, err
	}
	if !named.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", blobcodec.ErrCorrupt, cfpath)
	}
	if named.Size() > limit {
		return nil, fmt.Errorf("%w: %s holds %d bytes, more than the %d its chunk can take", blobcodec.ErrCorrupt, cfpath, named.Size(), limit)
	}
	f, err := os.Open(cfpath)
	if err != nil {
		return nil, err
	}
	data, err := readOpenedBlob(f, limit)
	if cerr := f.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("close %s: %w", cfpath, cerr)
	}
	return data, err
}

// readOpenedBlob reads at most limit bytes of f. It checks the opened handle
// rather than comparing it with the Lstat: on Windows that comparison opens
// the path again, which costs a second open per chunk and sees the same
// swapped file anyway.
func readOpenedBlob(f *os.File, limit int64) ([]byte, error) {
	opened, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", f.Name(), err)
	}
	if !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", blobcodec.ErrCorrupt, f.Name())
	}
	if opened.Size() > limit {
		return nil, fmt.Errorf("%w: %s holds %d bytes, more than the %d its chunk can take", blobcodec.ErrCorrupt, f.Name(), opened.Size(), limit)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: %s grew past the %d bytes its chunk can take", blobcodec.ErrCorrupt, f.Name(), limit)
	}
	return data, nil
}
