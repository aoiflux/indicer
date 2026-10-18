package index

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"time"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/entropy"
	"indicer/lib/parser/core"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/vmihailenco/msgpack/v5"
)

// headLen is how much of a file's content its type is detected from.
const headLen = 4096

// file indexes one record of the walk. Directories and special files are
// counted; files, symlinks and streams are indexed, each under its own ID.
func (ix *indexer) file(_ core.PartitionInfo, rec core.FileRecord) error {
	if err := ix.failure(); err != nil {
		return err
	}
	op := ix.part
	switch rec.Kind {
	case core.KindDir:
		ix.cov.DirsSeen++
		return nil
	case core.KindOther:
		ix.cov.OtherSeen++
		return nil
	case core.KindSymlink:
		ix.cov.Symlinks++
	default:
		ix.cov.FilesSeen++
	}
	if rec.IsDeleted {
		ix.cov.Deleted++
	}
	iid := op.nextID(ix.opts.EvidenceID, rec)
	f := ix.newRecord(op, rec)

	if rec.Kind == core.KindSymlink {
		// A symlink's "content" is its target, which the record carries.
		f.Content = &structs.ContentRef{Kind: structs.ContentUnavailable}
		return ix.write(op, iid, &f, nil)
	}
	c := rec.Content
	switch {
	case rec.Size == 0:
		f.Content = &structs.ContentRef{Kind: structs.ContentNone}
		f.FileHash = ix.hashOf(nil)
		f.IndexedType = util.DetectFileType(rec.Path, nil)
		ix.setEntropy(&f, nil)
		return ix.write(op, iid, &f, nil)

	case c.Kind == core.ContentExtents:
		ext, data := extentsOf(c)
		if data > 1 {
			ix.cov.Fragmented++
			f.IsFragmented = true
		}
		if c.HoleBytes > 0 {
			ix.cov.Sparse++
		}
		f.Content = &structs.ContentRef{Kind: structs.ContentExtents, Located: c.Located, Extents: ext}
		return ix.submit(op, iid, f, ext)

	case c.Kind == core.ContentInline && len(c.Inline) <= structs.MaxInlineBytes:
		ix.cov.Inline++
		data := bytes.Clone(c.Inline)
		f.Content = &structs.ContentRef{Kind: structs.ContentInline, Located: int64(len(data)), Inline: data}
		f.FileHash = ix.hashOf(data)
		ix.setEntropy(&f, data)
		f.IndexedType = util.DetectFileType(rec.Path, data[:min(len(data), headLen)])
		return ix.write(op, iid, &f, nil)

	case c.Kind == core.ContentInline:
		// Too large to keep in the record: stored as an object (D10).
		data := c.Inline
		return ix.materialize(op, iid, &f, int64(len(data)), "inline", func() (sequentialSource, error) {
			return newSequentialSource(bytes.NewReader(data), nil), nil
		})

	case c.Kind == core.ContentDecoded && rec.Open != nil:
		return ix.materialize(op, iid, &f, rec.Size, string(rec.FSType)+" decoded", func() (sequentialSource, error) {
			rc, err := rec.Open()
			if err != nil {
				return sequentialSource{}, err
			}
			return newSequentialSource(rc, rc), nil
		})
	}
	ix.cov.Unavailable++
	f.Content = &structs.ContentRef{Kind: structs.ContentUnavailable}
	return ix.write(op, iid, &f, nil)
}

// setEntropy records the entropy of data, when the pass computes it.
func (ix *indexer) setEntropy(f *structs.IndexedFile, data []byte) {
	if ix.opts.Entropy {
		v := entropy.Of(data)
		f.Entropy = &v
	}
}

// nextID returns the record's deterministic ID, counting earlier records of
// the partition that hashed to the same one.
func (op *openPartition) nextID(eid []byte, rec core.FileRecord) []byte {
	p := op.info
	for collision := uint32(0); ; collision++ {
		id := IndexedID(eid, p.Index, p.Offset, rec.Inode, rec.Path, rec.IsDeleted, collision)
		k := binary.BigEndian.Uint64(id)
		if _, dup := op.seen[k]; !dup {
			op.seen[k] = struct{}{}
			return id
		}
	}
}

// newRecord fills the fields every indexed record carries.
func (ix *indexer) newRecord(op *openPartition, rec core.FileRecord) structs.IndexedFile {
	f := structs.NewIndexedFile(rec.Path, 0, rec.Size, "", rec.IsDeleted, "")
	f.Times = timestampsOf(rec.MACB)
	f.EvidenceID = ix.opts.EvidenceID
	f.PartitionID = op.id
	f.Path = rec.Path
	f.Stream = rec.Stream
	f.NodeKind = rec.Kind.String()
	f.Inode = rec.Inode
	f.Seq = rec.Seq
	f.Target = rec.Target
	f.Links = rec.Links
	f.Prov = uint32(rec.Prov)
	f.Confidence = uint8(rec.Content.Confidence)
	return f
}

// extentsOf converts content extents to the record's form, and counts the
// data extents.
func extentsOf(c core.Content) ([]structs.Extent, int) {
	out := make([]structs.Extent, len(c.Extents))
	data := 0
	for i, e := range c.Extents {
		out[i] = structs.Extent{FileOffset: e.FileOffset, Length: e.Length, Kind: uint8(e.Kind)}
		if e.Kind == core.ExtentData {
			out[i].Offset = e.ImageOffset
			data++
		}
	}
	return out, data
}

// dataSpan is the evidence range from the first data extent's start to the
// last one's end.
func dataSpan(ext []structs.Extent) (lo, hi int64, ok bool) {
	for _, e := range ext {
		if e.Kind != structs.ExtentData {
			continue
		}
		if !ok {
			lo, ok = e.Offset, true
		}
		hi = max(hi, e.Offset+e.Length)
	}
	return lo, hi, ok
}

func (ix *indexer) hashOf(data []byte) string {
	h := ix.newHash()
	h.Write(data)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// kv is one more entry a record's write carries.
type kv struct{ k, v []byte }

// memberEntry is the record's member key in its partition and its value.
// Until W7 moves search and near onto the reader, the value is the legacy
// evidence range: from the first data extent to the end of the last, or
// empty for content that is not an evidence range.
func memberEntry(op *openPartition, iid []byte, ext []structs.Extent) (key, val []byte, err error) {
	member := structs.InternalOffset{Start: 0, End: -1}
	if lo, hi, ok := dataSpan(ext); ok {
		member = structs.InternalOffset{Start: lo, End: hi - 1}
	}
	val, err = msgpack.Marshal(member)
	return dbio.PartitionMemberKey(op.key, base64.StdEncoding.EncodeToString(iid)), val, err
}

// write adds an indexed record to its partition's batch: the member key
// first, then extra entries and extent pages, the record and its hash
// lookup, so a member key exists for anything a pass that stops part way
// leaves behind (the delete finds records through their members).
func (ix *indexer) write(op *openPartition, iid []byte, f *structs.IndexedFile, ext []structs.Extent, extra ...kv) error {
	if lo, _, ok := dataSpan(ext); ok {
		f.Start = lo
	}
	mkey, mval, err := memberEntry(op, iid, ext)
	if err != nil {
		return err
	}
	b := op.batch
	if err := b.Set(mkey, mval); err != nil {
		return err
	}
	for _, e := range extra {
		if err := b.Set(e.k, e.v); err != nil {
			return err
		}
	}
	if len(ext) > structs.MaxRecordExtents {
		pages := (len(ext) + structs.MaxRecordExtents - 1) / structs.MaxRecordExtents
		for p := range pages {
			page := ext[p*structs.MaxRecordExtents : min((p+1)*structs.MaxRecordExtents, len(ext))]
			data, err := dbio.EncodeRecord(page)
			if err != nil {
				return err
			}
			if err := b.Set(dbio.ExtentPageKey(iid, p), data); err != nil {
				return err
			}
		}
		f.Content.Extents = nil
		f.Content.ExtentPages = pages
	}
	ikey := dbio.CanonicalIndexedKey(iid)
	data, err := dbio.EncodeRecord(f)
	if err != nil {
		return err
	}
	if err := b.Set(ikey, data); err != nil {
		return err
	}
	if f.FileHash != "" {
		lookup := util.AppendToBytesSlice(cnst.IdxFileHashLookupNamespace, f.FileHash)
		if err := b.Set(dbio.HashLookupMemberKey(lookup, ikey), []byte{}); err != nil {
			return err
		}
	}
	op.indexed.Add(1)
	return nil
}

// timestampsOf converts a node's times to the stored form, nil when it has
// none, so such a record encodes as it did before timestamps existed.
func timestampsOf(m core.MACB) *structs.Timestamps {
	nanos := func(t *time.Time) *int64 {
		if t == nil {
			return nil
		}
		n := t.UnixNano()
		return &n
	}
	ts := &structs.Timestamps{Modified: nanos(m.Modified), Accessed: nanos(m.Accessed), Changed: nanos(m.Changed), Born: nanos(m.Born)}
	if !ts.Any() {
		return nil
	}
	return ts
}
