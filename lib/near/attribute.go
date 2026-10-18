package near

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"

	"indicer/lib/dbio"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// target is an object an occurrence is attributed to, and the occurrence's
// position in it: an offset of its evidence, or of its materialised object.
type target struct {
	key  []byte
	pos  int64
	kind targetKind
	ev   *evidenceIndex // the evidence holding it; nil for a materialised object
	span interval       // a partition's or legacy file's evidence range
	file *fileInfo      // an indexed file in the evidence
	obj  *structs.ObjectRecord
}

type targetKind int

const (
	kindEvidence targetKind = iota
	kindPartition
	kindFile
	kindObject
)

// ownerInfo is what a reverse relation's owner ID names: a stored evidence,
// a materialised object of one, or nothing searchable.
type ownerInfo struct {
	once sync.Once
	ev   *evidenceIndex
	obj  *structs.ObjectRecord
	err  error
}

// attribute returns the objects the occurrence o lies in: the indexed files
// of its evidence that hold its bytes, else the partitions, else the
// evidence itself; or the file a materialised object belongs to.
func (r *run) attribute(ctx context.Context, o occurrence) ([]target, error) {
	r.mu.Lock()
	info := r.owners[o.owner]
	if info == nil {
		info = &ownerInfo{}
		r.owners[o.owner] = info
	}
	r.mu.Unlock()
	info.once.Do(func() { info.ev, info.obj, info.err = r.resolveOwner(ctx, []byte(o.owner)) })
	switch {
	case info.err != nil:
		return nil, info.err
	case info.ev != nil:
		return info.ev.find(o.offset)
	case info.obj != nil:
		return []target{{key: dbio.CanonicalIndexedKey(info.obj.IndexedID), pos: o.offset, kind: kindObject, obj: info.obj}}, nil
	}
	r.stale.Add(1)
	return nil, nil
}

func (r *run) resolveOwner(ctx context.Context, id []byte) (*evidenceIndex, *structs.ObjectRecord, error) {
	e, err := dbio.GetEvidenceFile(id, r.db)
	switch {
	case err == nil:
		if !storedEvidence(e) {
			return nil, nil, nil
		}
		ev, err := buildEvidenceIndex(ctx, r.db, id, e)
		return ev, nil, err
	case !errors.Is(err, badger.ErrKeyNotFound):
		return nil, nil, err
	}
	obj, err := dbio.GetObject(r.db, id)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	pe, err := dbio.GetEvidenceFile(obj.EvidenceID, r.db)
	if errors.Is(err, badger.ErrKeyNotFound) || (err == nil && !storedEvidence(pe)) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return nil, &obj, nil
}

// interval is the evidence range [start, end) of a partition or file.
type interval struct {
	start, end int64
	key        []byte
}

// evidenceIndex locates a stored evidence's partitions and indexed files by
// evidence offset, so an occurrence is attributed without decoding records
// for every chunk (SRC-5). It is built once per search, for evidence an
// occurrence lies in.
type evidenceIndex struct {
	db         *badger.DB
	id, key    []byte
	start, end int64
	stride     int64
	parts      []interval
	files      []interval // sorted by start
	maxEnd     []int64    // maxEnd[i] is the largest end of files[:i+1]

	mu    sync.Mutex
	infos map[string]*fileInfo
}

// fileInfo is what attribution needs of an indexed file's record: its data
// extents, or none for a legacy single range.
type fileInfo struct {
	once    sync.Once
	extents []structs.Extent // data extents; nil for a single range
	err     error
}

func buildEvidenceIndex(ctx context.Context, db *badger.DB, id []byte, e structs.EvidenceFile) (*evidenceIndex, error) {
	stride, err := dbio.EvidenceChunkSize(db, id, e.Start+e.Size)
	if err != nil {
		return nil, err
	}
	ev := &evidenceIndex{db: db, id: id, key: dbio.CanonicalEvidenceKey(id), start: e.Start, end: e.Start + e.Size, stride: max(stride, 1), infos: map[string]*fileInfo{}}
	if e.Index != nil && e.Index.State == structs.IndexInvalid {
		return ev, nil // a quarantined index names nothing
	}
	for pidB64 := range e.InternalObjects {
		pid, err := base64.StdEncoding.DecodeString(pidB64)
		if err != nil {
			return nil, fmt.Errorf("partition ID %q of evidence %x: %w", pidB64, id, err)
		}
		pkey := dbio.CanonicalPartitionKey(pid)
		prec, err := dbio.GetPartitionFile(pkey, db)
		if errors.Is(err, badger.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		ev.parts = append(ev.parts, interval{start: prec.Start, end: prec.Start + prec.Size, key: pkey})
		err = dbio.ForEachPartitionMember(db, pkey, prec, func(iidB64 string, off structs.InternalOffset) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if off.End < off.Start {
				return nil // content that is not an evidence range
			}
			iid, err := base64.StdEncoding.DecodeString(iidB64)
			if err != nil {
				return nil
			}
			ev.files = append(ev.files, interval{start: off.Start, end: off.End + 1, key: dbio.CanonicalIndexedKey(iid)})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(ev.files, func(a, b interval) int {
		switch {
		case a.start < b.start:
			return -1
		case a.start > b.start:
			return 1
		}
		return 0
	})
	ev.maxEnd = make([]int64, len(ev.files))
	for i, f := range ev.files {
		ev.maxEnd[i] = f.end
		if i > 0 {
			ev.maxEnd[i] = max(f.end, ev.maxEnd[i-1])
		}
	}
	return ev, nil
}

// find attributes the chunk at evidence offset x.
func (ev *evidenceIndex) find(x int64) ([]target, error) {
	hi := min(x+ev.stride, ev.end)
	var out []target
	i := sort.Search(len(ev.files), func(i int) bool { return ev.files[i].start >= hi })
	for j := i - 1; j >= 0 && ev.maxEnd[j] > x; j-- {
		f := ev.files[j]
		if f.end <= x {
			continue
		}
		info := ev.fileInfo(f.key)
		ok, err := info.holds(x, hi)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, target{key: f.key, pos: x, kind: kindFile, ev: ev, span: f, file: info})
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	for _, p := range ev.parts {
		if p.start < hi && p.end > x {
			out = append(out, target{key: p.key, pos: x, kind: kindPartition, ev: ev, span: p})
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	return []target{{key: ev.key, pos: x, kind: kindEvidence, ev: ev, span: interval{start: ev.start, end: ev.end, key: ev.key}}}, nil
}

func (ev *evidenceIndex) fileInfo(key []byte) *fileInfo {
	ev.mu.Lock()
	info := ev.infos[string(key)]
	if info == nil {
		info = &fileInfo{}
		ev.infos[string(key)] = info
	}
	ev.mu.Unlock()
	info.once.Do(func() {
		rec, err := dbio.GetIndexedFile(key, ev.db)
		if err != nil {
			info.err = err
			return
		}
		ext, err := dbio.IndexedExtents(ev.db, key, rec)
		if err != nil {
			info.err = err
			return
		}
		for _, e := range ext {
			if e.Kind == structs.ExtentData {
				info.extents = append(info.extents, e)
			}
		}
		if ext != nil && info.extents == nil {
			info.extents = []structs.Extent{} // all holes: no evidence bytes
		}
	})
	return info
}

// holds reports whether the file's bytes include some of [lo, hi): any of
// its data extents does, or, for a single range, the range it was found by.
func (info *fileInfo) holds(lo, hi int64) (bool, error) {
	if info.err != nil {
		return false, info.err
	}
	if info.extents == nil {
		return true, nil
	}
	for _, e := range info.extents {
		if e.Offset < hi && e.Offset+e.Length > lo {
			return true, nil
		}
	}
	return false, nil
}

// chunks is how many of its evidence's chunks the object's bytes lie in:
// what its matched chunks are a share of.
func (t target) chunks() int64 {
	switch t.kind {
	case kindObject:
		return ceilDiv(t.obj.Size, max(t.obj.ChunkSize, 1))
	case kindFile:
		if t.file.extents != nil {
			return chunksOfExtents(t.file.extents, t.ev.stride)
		}
	}
	return chunksOfRange(t.span.start, t.span.end, t.ev.stride)
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }

func chunksOfRange(start, end, stride int64) int64 {
	if end <= start {
		return 0
	}
	return (end-1)/stride - start/stride + 1
}

// chunksOfExtents counts the distinct chunks the data extents lie in.
func chunksOfExtents(ext []structs.Extent, stride int64) int64 {
	type span struct{ lo, hi int64 } // chunk indexes, inclusive
	spans := make([]span, 0, len(ext))
	for _, e := range ext {
		if e.Length > 0 {
			spans = append(spans, span{e.Offset / stride, (e.Offset + e.Length - 1) / stride})
		}
	}
	slices.SortFunc(spans, func(a, b span) int {
		switch {
		case a.lo < b.lo:
			return -1
		case a.lo > b.lo:
			return 1
		}
		return 0
	})
	var n int64
	last := int64(-1)
	for _, s := range spans {
		lo := max(s.lo, last+1)
		if s.hi >= lo {
			n += s.hi - lo + 1
			last = s.hi
		}
	}
	return n
}
