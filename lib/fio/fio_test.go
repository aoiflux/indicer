package fio

import (
	"bytes"
	"errors"
	"hash"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"indicer/lib/cnst"
	"indicer/lib/fsx"

	"github.com/zeebo/blake3"
)

// TestWriteChonkConcurrentSameChunk: ingests running at once (two ingests of
// the same content, or an ingest's index pass storing decoded content beside
// its chunk pass) write the same new chunk at the same time. The second
// writer found the first one's file, took it for a torn blob and renamed a
// replacement over it while the first still held it open, which Windows
// refuses, so the ingest failed.
func TestWriteChonkConcurrentSameChunk(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blob"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Many writers, each writing the same chunks in the same order, as
	// identical ingests do.
	const writers, chunks = 8, 64
	data := bytes.Repeat([]byte("concurrent chunk "), 256)
	key := func(c int) []byte {
		return append(append([]byte{}, cnst.ChonkNamespace...), bytes.Repeat([]byte{byte(c), byte(c >> 8)}, 16)...)
	}
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range chunks {
				if _, err := WriteChonk(dir, data, key(c), blobHash); err != nil {
					errs[w] = err
					return
				}
			}
		}()
	}
	wg.Wait()
	for w, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", w, err)
		}
	}
	for c := range chunks {
		path, err := WriteChonk(dir, data, key(c), blobHash)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(string(path)); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("chunk %d: blob holds %d bytes, %v", c, len(got), err)
		}
	}
}

// TestWriteChonkWaitsForAnOpenWriter makes the race above deterministic: the
// first writer of a blob is held while it has the new file open, and
// replacing a file another writer holds open fails, as it does on Windows
// (and for as long as an antivirus scanner holds a new file there).
func TestWriteChonkWaitsForAnOpenWriter(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blob"), 0o755); err != nil {
		t.Fatal(err)
	}
	prevNew, prevReplace := writeNewFile, replaceFile
	t.Cleanup(func() { writeNewFile, replaceFile = prevNew, prevReplace })
	created, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	open := map[string]bool{}
	first := true
	writeNewFile = func(path string, data []byte, perm os.FileMode) error {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			return err
		}
		mu.Lock()
		open[path] = true
		hold := first
		first = false
		mu.Unlock()
		if hold {
			close(created)
			<-release
		}
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		mu.Lock()
		delete(open, path)
		mu.Unlock()
		return err
	}
	replaceFile = func(path string, data []byte, perm os.FileMode) error {
		mu.Lock()
		busy := open[path]
		mu.Unlock()
		if busy {
			return errors.New("the file is in use by another process")
		}
		return prevReplace(path, data, perm)
	}

	ckey := append(append([]byte{}, cnst.ChonkNamespace...), bytes.Repeat([]byte{9}, 32)...)
	data := []byte("one chunk, two writers")
	firstErr := make(chan error, 1)
	go func() {
		_, err := WriteChonk(dir, data, ckey, blobHash)
		firstErr <- err
	}()
	<-created
	secondErr := make(chan error, 1)
	go func() {
		_, err := WriteChonk(dir, data, ckey, blobHash)
		secondErr <- err
	}()
	// The second writer must not get anywhere until the first is done.
	select {
	case err := <-secondErr:
		close(release)
		<-firstErr
		t.Fatalf("the second writer finished while the first held the file open: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-firstErr; err != nil {
		t.Fatalf("first writer: %v", err)
	}
	if err := <-secondErr; err != nil {
		t.Fatalf("second writer: %v", err)
	}
}

// TestWriteChonkCreatesAndReplaces covers both write paths of WriteChonk: a
// new blob is created in place, and an existing file under the blob's name
// (a blob torn by a crash, or truncated) is replaced whole.
func TestWriteChonkCreatesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	blobDir := filepath.Join(dir, "blob")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ckey := append(append([]byte{}, cnst.ChonkNamespace...), bytes.Repeat([]byte{7}, 32)...)
	data := bytes.Repeat([]byte("chunk"), 1000)

	path, err := WriteChonk(dir, data, ckey, blobHash)
	if err != nil {
		t.Fatal(err)
	}
	assertBlob := func(stage string) {
		t.Helper()
		got, err := os.ReadFile(string(path))
		if err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("%s: blob holds %d bytes, want the %d written", stage, len(got), len(data))
		}
	}
	assertBlob("new blob")

	if err := os.Truncate(string(path), int64(len(data)/2)); err != nil {
		t.Fatal(err)
	}
	again, err := WriteChonk(dir, data, ckey, blobHash)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(path) {
		t.Fatalf("rewrite went to %s, want %s", again, path)
	}
	assertBlob("rewritten blob")

	entries, err := os.ReadDir(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if fsx.IsTempName(e.Name()) {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

// blobHash is the chunk hash the tests name blobs with.
func blobHash() hash.Hash { return blake3.New() }
