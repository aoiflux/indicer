package store

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"errors"
	"hash"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"indicer/internal/testutil"
	"indicer/internal/testutil/fsfixture"
	"indicer/internal/testutil/leakcheck"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/parser/image"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/zeebo/blake3"
)

func newBlake3() hash.Hash { return blake3.New() }

func sum(b []byte) []byte {
	h := blake3.New()
	h.Write(b)
	return h.Sum(nil)
}

// The feed delivers every chunk once, in any order, and the tap hashes the
// bytes in order, whatever the number of readers.
func TestChunkFeedDigests(t *testing.T) {
	content := testutil.RandomBytes(3, 10*4096+123)
	ranges := []ByteRange{{0, 512}, {4000, 9000}, {5000, 100}, {40000, 100000}}
	for _, readers := range []int{1, 4} {
		tap := newDigestTap(newBlake3, ranges, nil)
		f := startChunkFeed(context.Background(), feedOptions{src: bytes.NewReader(content), end: int64(len(content)), chunk: 4096, readers: readers, slots: 3, tap: tap})
		got := make([]byte, len(content))
		seen := 0
		for c := range f.chunks() {
			copy(got[c.off:], c.data)
			seen++
			c.release()
		}
		if err := f.wait(); err != nil {
			t.Fatal(err)
		}
		if seen != 11 || !bytes.Equal(got, content) {
			t.Fatalf("readers=%d: %d chunks, content equal %v", readers, seen, bytes.Equal(got, content))
		}
		if !bytes.Equal(tap.plain.Sum(nil), sum(content)) {
			t.Fatalf("readers=%d: plain hash differs", readers)
		}
		for _, d := range tap.rangeDigests() {
			end := min(d.Offset+d.Length, int64(len(content)))
			if !bytes.Equal(d.Hash, sum(content[d.Offset:end])) {
				t.Fatalf("readers=%d: range %+v digest differs", readers, d.ByteRange)
			}
		}
	}
}

// The ring bounds what the feed holds: with every slot unreleased, no further
// chunk is read.
func TestChunkFeedRingIsBounded(t *testing.T) {
	content := make([]byte, 20*4096)
	var reads atomic.Int64
	src := readerFunc(func(p []byte, off int64) (int, error) {
		reads.Add(1)
		return bytes.NewReader(content).ReadAt(p, off)
	})
	f := startChunkFeed(context.Background(), feedOptions{src: src, end: int64(len(content)), chunk: 4096, readers: 2, slots: 3})
	var held []*feedChunk
	for range 3 {
		held = append(held, <-f.chunks())
	}
	select {
	case c := <-f.chunks():
		t.Fatalf("a fourth chunk (offset %d) arrived with three slots held", c.off)
	case <-time.After(100 * time.Millisecond):
	}
	if n := reads.Load(); n != 3 {
		t.Fatalf("%d reads with three slots held, want 3", n)
	}
	for _, c := range held {
		c.release()
	}
	n := 3
	for c := range f.chunks() {
		n++
		c.release()
	}
	if err := f.wait(); err != nil || n != 20 {
		t.Fatalf("%d chunks, err %v", n, err)
	}
}

type readerFunc func(p []byte, off int64) (int, error)

func (f readerFunc) ReadAt(p []byte, off int64) (int, error) { return f(p, off) }

// A read error ends the pass with the offset it happened at, and the
// evidence is not taken as stored.
func TestIngestChunksSourceReadError(t *testing.T) {
	for _, mode := range []string{cnst.StorePipelineBatch, cnst.StorePipelineStaged} {
		t.Run(mode, func(t *testing.T) {
			db := testdb.Open(t, testdb.Default)
			util.SetChonkSize(4)
			setPipelineMode(t, mode)
			if err := util.EnsureBlobPath(db.Opts().Dir); err != nil {
				t.Fatal(err)
			}
			content := testutil.RandomBytes(5, 64*4096)
			bad := int64(37*4096 + 100)
			src := readerFunc(func(p []byte, off int64) (int, error) {
				if off <= bad && bad < off+int64(len(p)) {
					n := copy(p, content[off:bad])
					return n, errors.New("device gone")
				}
				return bytes.NewReader(content).ReadAt(p, off)
			})
			done := make(chan error, 1)
			go func() {
				_, err := IngestChunks(t.Context(), ChunkPass{DB: db.DB, ID: []byte("bad-source"), Source: src, Size: int64(len(content)), Parallel: true, Quiet: true})
				done <- err
			}()
			var err error
			select {
			case err = <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("the pass did not end after a read error")
			}
			var rerr *SourceReadError
			if !errors.As(err, &rerr) || !errors.Is(err, ErrSourceRead) || rerr.Offset != bad {
				t.Fatalf("err = %v, want a SourceReadError at %d", err, bad)
			}
		})
	}
}

// The EWF digests are checked over exactly the decoded bytes stored.
func TestIngestChunksVerifiesEWF(t *testing.T) {
	var m fsfixture.Manifest
	for _, x := range fsfixture.All(t) {
		if x.Name == "ewf/fat12_multiseg" {
			m = x
		}
	}
	src, err := image.OpenSource(t.Context(), fsfixture.Open(t, m), image.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Image.Close()

	db := testdb.Open(t, testdb.Default)
	util.SetChonkSize(4)
	if err := util.EnsureBlobPath(db.Opts().Dir); err != nil {
		t.Fatal(err)
	}
	res, err := IngestChunks(t.Context(), ChunkPass{DB: db.DB, ID: []byte("ewf-evidence"), Source: src.Image, Size: src.Image.Size(), Parallel: true, EWF: src.Meta.EWF, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.EWF == nil || !res.EWF.OK() || res.EWF.Bytes != m.Size {
		t.Fatalf("EWF result %+v, want verified over %d bytes", res.EWF, m.Size)
	}
	decoded := make([]byte, m.Size)
	if _, err := src.Image.ReadAt(decoded, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !bytes.Equal(res.PlainHash, sum(decoded)) {
		t.Fatal("plain hash is not the decoded image's")
	}
	md, sh := md5.Sum(decoded), sha1.Sum(decoded)
	if !bytes.Equal(md[:], src.Meta.EWF.MD5) || (src.Meta.EWF.SHA1 != nil && !bytes.Equal(sh[:], src.Meta.EWF.SHA1)) {
		t.Fatal("fixture's stored digests are not of its decoded image")
	}

	// The stored evidence reads back as the decoded image.
	rid := []byte("ewf-evidence")
	if err := dbio.CreateEvidence(db.DB, rid, newEvidenceRecord(m.Size)); err != nil {
		t.Fatal(err)
	}
	ev, err := dbio.OpenEvidenceChunks(db.DB, rid, dbio.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	back := make([]byte, m.Size)
	if _, err := ev.ReadAt(back, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !bytes.Equal(back, decoded) {
		t.Fatal("stored evidence does not read back as the decoded image")
	}
}

func setPipelineMode(t *testing.T, mode string) {
	t.Helper()
	prev := cnst.StorePipelineMode
	cnst.StorePipelineMode = mode
	t.Cleanup(func() { cnst.StorePipelineMode = prev })
}

func newEvidenceRecord(size int64) structs.EvidenceFile {
	return structs.NewEvidenceFile("evidence", 0, size, nil, cnst.UnknownEvidenceType, "")
}

func TestMain(m *testing.M) {
	code := leakcheck.Run(m)
	fsfixture.Cleanup()
	os.Exit(code)
}
