package index

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"

	"indicer/lib/dbio"
	"indicer/lib/entropy"
	"indicer/lib/structs"
	"indicer/lib/util"
)

// hashBufSize is how much of a file a hash worker reads at a time.
const hashBufSize = 1 << 20

// hashJob is an extents file waiting for its content hash.
type hashJob struct {
	op  *openPartition
	iid []byte
	rec structs.IndexedFile
	ext []structs.Extent
}

// submit queues an extents file; a hash worker reads its content from the
// image, hashes it and writes the record.
func (ix *indexer) submit(op *openPartition, iid []byte, f structs.IndexedFile, ext []structs.Extent) error {
	op.pending.Add(1)
	select {
	case ix.jobs <- hashJob{op: op, iid: iid, rec: f, ext: ext}:
		return nil
	case <-ix.ctx.Done():
		op.pending.Done()
		return context.Cause(ix.ctx)
	}
}

func (ix *indexer) hashWorker() {
	defer ix.workers.Done()
	buf := make([]byte, hashBufSize)
	for j := range ix.jobs {
		if ix.ctx.Err() == nil {
			if err := ix.hashAndWrite(j, buf); err != nil {
				ix.fail(err)
			}
		}
		j.op.pending.Done()
	}
}

// hashAndWrite hashes the file's located bytes, holes read as zeros. The
// extents were checked against the image when the scan built them, so a read
// that fails is the evidence failing, and stops the pass (PAR-32).
func (ix *indexer) hashAndWrite(j hashJob, buf []byte) error {
	r, err := dbio.NewExtentReader(ix.opts.Image, j.ext)
	if err != nil {
		return fmt.Errorf("index: %s: %w", j.rec.Path, err)
	}
	h := ix.newHash()
	var counts *entropy.Counter
	if ix.opts.Entropy {
		counts = new(entropy.Counter)
	}
	var head []byte
	for off, n := int64(0), 0; off < r.Size(); n++ {
		if n%64 == 63 && ix.ctx.Err() != nil {
			return context.Cause(ix.ctx)
		}
		want := int(min(int64(len(buf)), r.Size()-off))
		m, err := r.ReadAt(buf[:want], off)
		if m < want {
			return fmt.Errorf("index: %s: read content at file offset %d: %w", j.rec.Path, off+int64(m), err)
		}
		if head == nil {
			head = bytes.Clone(buf[:min(m, headLen)])
		}
		h.Write(buf[:m])
		if counts != nil {
			counts.Add(buf[:m])
		}
		off += int64(m)
	}
	if counts != nil {
		v := counts.Value()
		j.rec.Entropy = &v
	}
	j.rec.FileHash = base64.StdEncoding.EncodeToString(h.Sum(nil))
	j.rec.IndexedType = util.DetectFileType(j.rec.Path, head)
	return ix.write(j.op, j.iid, &j.rec, j.ext)
}
