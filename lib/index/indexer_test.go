package index

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"testing"

	"indicer/internal/testutil/fsfixture"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/parser/core"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

func TestMain(m *testing.M) {
	code := m.Run()
	fsfixture.Cleanup()
	os.Exit(code)
}

// memImage is an image held in memory.
type memImage []byte

func (m memImage) ReadAt(p []byte, off int64) (int, error) { return bytes.NewReader(m).ReadAt(p, off) }
func (m memImage) Size() int64                             { return int64(len(m)) }
func (m memImage) Format() core.ImageFormat                { return core.FormatRaw }
func (m memImage) Close() error                            { return nil }

var testEID = []byte("0123456789abcdef")

// openIndexDB opens a database in 4 KiB chunks, with an evidence record for
// testEID of size bytes.
func openIndexDB(t *testing.T, size int64) *badger.DB {
	t.Helper()
	d := testdb.Open(t, testdb.Default)
	util.SetChonkSize(4)
	if err := util.EnsureBlobPath(d.Root); err != nil {
		t.Fatal(err)
	}
	if err := dbio.CreateEvidence(d.DB, testEID, structs.NewEvidenceFile("ev.img", 0, size, nil, cnst.UnknownEvidenceType, "")); err != nil {
		t.Fatal(err)
	}
	return d.DB
}

func randomBytes(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// failingReader returns n bytes, then an error.
type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func (r *failingReader) Close() error { return nil }

func opener(rc io.ReadCloser) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return rc, nil }
}

// indexed is what a pass wrote for one record.
type indexed struct {
	id  []byte
	rec structs.IndexedFile
}

// recordsOf reads every indexed record of the evidence through its
// partitions' members.
func recordsOf(t *testing.T, db *badger.DB, eid []byte) map[string][]indexed {
	t.Helper()
	e, err := dbio.GetEvidenceFile(eid, db)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]indexed{}
	for pidB64 := range e.InternalObjects {
		pid, err := base64.StdEncoding.DecodeString(pidB64)
		if err != nil {
			t.Fatal(err)
		}
		prec, err := dbio.GetPartitionFile(pid, db)
		if err != nil {
			t.Fatalf("partition %s: %v", pidB64, err)
		}
		err = dbio.ForEachPartitionMember(db, pid, prec, func(iidB64 string, _ structs.InternalOffset) error {
			iid, err := base64.StdEncoding.DecodeString(iidB64)
			if err != nil {
				return err
			}
			rec, err := dbio.GetIndexedFile(iid, db)
			if err != nil {
				return err
			}
			out[rec.Path] = append(out[rec.Path], indexed{id: iid, rec: rec})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// contentOf reads a record's located bytes the way the reader will.
func contentOf(t *testing.T, db *badger.DB, img io.ReaderAt, id []byte, rec structs.IndexedFile) []byte {
	t.Helper()
	c := rec.Content
	if c == nil {
		t.Fatalf("%s: record has no content reference", rec.Path)
	}
	switch c.Kind {
	case structs.ContentNone:
		return []byte{}
	case structs.ContentInline:
		return c.Inline
	case structs.ContentExtents:
		ext := c.Extents
		for p := range c.ExtentPages {
			var page []structs.Extent
			err := db.View(func(txn *badger.Txn) error { return dbio.GetRecordTxn(txn, dbio.ExtentPageKey(id, p), &page) })
			if err != nil {
				t.Fatalf("%s: extent page %d: %v", rec.Path, p, err)
			}
			ext = append(ext, page...)
		}
		r, err := dbio.NewExtentReader(img, ext)
		if err != nil {
			t.Fatalf("%s: %v", rec.Path, err)
		}
		if r.Size() != c.Located {
			t.Fatalf("%s: extents cover %d bytes, record says %d located", rec.Path, r.Size(), c.Located)
		}
		return readAll(t, rec.Path, r, r.Size())
	case structs.ContentMaterialized:
		ec, obj, err := dbio.OpenObjectChunks(db, c.ObjectID, dbio.ReadOptions{})
		if err != nil {
			t.Fatalf("%s: %v", rec.Path, err)
		}
		if obj.FileHash != rec.FileHash || !bytes.Equal(obj.IndexedID, id) || !bytes.Equal(obj.EvidenceID, rec.EvidenceID) {
			t.Fatalf("%s: object record %+v does not match its indexed record", rec.Path, obj)
		}
		return readAll(t, rec.Path, ec, obj.Size)
	}
	t.Fatalf("%s: content %q cannot be read", rec.Path, c.Kind)
	return nil
}

func readAll(t *testing.T, path string, r io.ReaderAt, size int64) []byte {
	t.Helper()
	b := make([]byte, size)
	if n, err := r.ReadAt(b, 0); int64(n) != size {
		t.Fatalf("%s: read %d of %d bytes: %v", path, n, size, err)
	}
	return b
}

func fileHash(db *badger.DB, b []byte) string {
	h := dbmeta.FileHasher(db)()
	h.Write(b)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

var (
	syntheticLarge   = randomBytes(20<<10, 2)
	syntheticDecoded = randomBytes(9000, 3)
)

// indexSynthetic indexes one partition of every content kind over a random
// image: extents with a hole, truncated extents, small and large inline data,
// decoded content that reads and that fails, an empty file, two deleted
// entries sharing a name and inode, a symlink and a directory.
func indexSynthetic(t *testing.T) (*badger.DB, memImage, map[string][]indexed, Result) {
	t.Helper()
	img := memImage(randomBytes(64<<10, 1))
	db := openIndexDB(t, img.Size())
	ix := newIndexer(t.Context(), Options{DB: db, EvidenceID: testEID, EvidenceName: "ev.img", Image: img, Workers: 2})

	large, decoded := syntheticLarge, syntheticDecoded
	part := core.PartitionInfo{Partition: core.Partition{Index: 0, Offset: 0, Size: img.Size(), Allocated: true}, FSType: core.FSExt, Status: core.PartIndexed}
	file := func(path string, size int64, c core.Content) core.FileRecord {
		c.Size = size
		return core.FileRecord{Name: path, Path: path, Kind: core.KindFile, Size: size, Content: c, Inode: uint64(len(path)), FSType: core.FSExt}
	}
	sparse := file("/sparse.bin", 300, core.Content{Kind: core.ContentExtents, Located: 300, DataBytes: 250, HoleBytes: 50, Extents: []core.Extent{
		{FileOffset: 0, Length: 100, ImageOffset: 1000},
		{FileOffset: 100, Length: 50, Kind: core.ExtentSparse},
		{FileOffset: 150, Length: 150, ImageOffset: 5000},
	}})
	truncated := file("/truncated.bin", 500, core.Content{Kind: core.ContentExtents, Located: 200, DataBytes: 200, Extents: []core.Extent{{FileOffset: 0, Length: 200, ImageOffset: 2000}}})
	small := file("/small.txt", 5, core.Content{Kind: core.ContentInline, Located: 5, Inline: []byte("hello")})
	big := file("/big.bin", int64(len(large)), core.Content{Kind: core.ContentInline, Located: int64(len(large)), Inline: large})
	dec := file("/z/decoded.bin", int64(len(decoded)), core.Content{Kind: core.ContentDecoded, Located: int64(len(decoded))})
	dec.Open = opener(io.NopCloser(bytes.NewReader(decoded)))
	broken := file("/z/broken.bin", int64(len(decoded)), core.Content{Kind: core.ContentDecoded, Located: int64(len(decoded))})
	broken.Open = opener(&failingReader{data: decoded[:3000], err: errors.New("lznt1: bad phrase")})
	empty := file("/empty.txt", 0, core.Content{})
	gone1 := file("/gone.txt", 3, core.Content{Kind: core.ContentInline, Located: 3, Inline: []byte("one")})
	gone1.IsDeleted = true
	gone2 := gone1
	gone2.Content.Inline = []byte("two")
	link := core.FileRecord{Path: "/link", Kind: core.KindSymlink, Size: 7, Target: "big.bin"}
	dir := core.FileRecord{Path: "/z", Kind: core.KindDir}

	if err := ix.beginPartition(part); err != nil {
		t.Fatal(err)
	}
	for _, rec := range []core.FileRecord{sparse, truncated, small, big, dec, broken, empty, gone1, gone2, link, dir} {
		if err := ix.file(part, rec); err != nil {
			t.Fatalf("%s: %v", rec.Path, err)
		}
	}
	if err := ix.endPartition(part, &core.IndexReport{FilesSeen: 9}); err != nil {
		t.Fatal(err)
	}
	if err := ix.stop(); err != nil {
		t.Fatal(err)
	}
	return db, img, recordsOf(t, db, testEID), ix.result(nil)
}

func TestIndexerContentKinds(t *testing.T) {
	db, img, recs, res := indexSynthetic(t)
	large, decoded := syntheticLarge, syntheticDecoded
	want := map[string]struct {
		kind    string
		content []byte
	}{
		"/sparse.bin":    {structs.ContentExtents, append(append(bytes.Clone(img[1000:1100]), make([]byte, 50)...), img[5000:5150]...)},
		"/truncated.bin": {structs.ContentExtents, img[2000:2200]},
		"/small.txt":     {structs.ContentInline, []byte("hello")},
		"/big.bin":       {structs.ContentMaterialized, large},
		"/z/decoded.bin": {structs.ContentMaterialized, decoded},
		"/empty.txt":     {structs.ContentNone, []byte{}},
	}
	for path, w := range want {
		got := recs[path]
		if len(got) != 1 {
			t.Fatalf("%s: %d records, want 1", path, len(got))
		}
		r := got[0].rec
		if r.Content.Kind != w.kind {
			t.Errorf("%s: content %q, want %q", path, r.Content.Kind, w.kind)
			continue
		}
		if b := contentOf(t, db, img, got[0].id, r); !bytes.Equal(b, w.content) {
			t.Errorf("%s: content differs (%d bytes, want %d)", path, len(b), len(w.content))
		}
		if r.FileHash != fileHash(db, w.content) {
			t.Errorf("%s: file hash is not the plain hash of its content", path)
		}
		if !bytes.Equal(r.EvidenceID, testEID) || r.Path != path || r.Name != path {
			t.Errorf("%s: evidence %x, path %q, name %q", path, r.EvidenceID, r.Path, r.Name)
		}
	}
	if r := recs["/truncated.bin"][0].rec; r.Size != 500 || r.Content.Located != 200 {
		t.Errorf("truncated: size %d, located %d; want 500, 200", r.Size, r.Content.Located)
	}

	// A decode that fails part way leaves the record metadata only, and no
	// relation of the half-stored object.
	b := recs["/z/broken.bin"]
	if len(b) != 1 || b[0].rec.Content.Kind != structs.ContentUnavailable || b[0].rec.FileHash != "" {
		t.Fatalf("broken: %+v, want one record, metadata only", b)
	}
	if n := countPrefix(t, db, dbio.RelationPrefix(b[0].id)); n != 0 {
		t.Errorf("broken: %d relations left of the half-stored object", n)
	}
	if _, err := dbio.GetObject(db, b[0].id); !errors.Is(err, badger.ErrKeyNotFound) {
		t.Errorf("broken: object record: %v, want none", err)
	}

	// Two deleted entries with one name and inode keep a record each.
	if g := recs["/gone.txt"]; len(g) != 2 || bytes.Equal(g[0].id, g[1].id) {
		t.Errorf("deleted entries sharing a name: %d records", len(g))
	}
	if l := recs["/link"]; len(l) != 1 || l[0].rec.Target != "big.bin" || l[0].rec.NodeKind != "symlink" || l[0].rec.Content.Kind != structs.ContentUnavailable {
		t.Errorf("symlink: %+v", l)
	}
	if _, ok := recs["/z"]; ok {
		t.Error("a directory was indexed")
	}

	cov := res.Coverage
	if cov.FilesSeen != 9 || cov.Symlinks != 1 || cov.DirsSeen != 1 || cov.FilesIndexed != 10 ||
		cov.Materialized != 2 || cov.Inline != 3 || cov.Unavailable != 1 || cov.Sparse != 1 || cov.Fragmented != 1 || cov.Deleted != 2 {
		t.Errorf("coverage %+v", cov)
	}
	if res.State != structs.IndexPartial || res.Reason == "" {
		t.Errorf("state %q (%q), want partial: a decode failed", res.State, res.Reason)
	}

	// The partition's record counts its members and keeps its report.
	prec, err := dbio.GetPartitionFile(res.Partitions[0].ID, db)
	if err != nil {
		t.Fatal(err)
	}
	if prec.MemberCount != 10 || prec.Partition == nil || prec.Partition.Status != string(core.PartIndexed) || prec.IndexedType != "ext" {
		t.Errorf("partition record: members %d, meta %+v, type %q", prec.MemberCount, prec.Partition, prec.IndexedType)
	}
	var rep structs.PartitionReport
	if err := db.View(func(txn *badger.Txn) error {
		return dbio.GetRecordTxn(txn, dbio.PartitionReportKey(res.Partitions[0].ID), &rep)
	}); err != nil || rep.FilesIndexed != 10 || rep.Materialized != 2 || rep.FilesSeen != 9 {
		t.Errorf("report %+v, %v", rep, err)
	}

	// Every hash lookup names its record.
	for path, rs := range recs {
		for _, r := range rs {
			if r.rec.FileHash == "" {
				continue
			}
			ids, err := dbio.GetHashLookupUUIDsByKey(util.AppendToBytesSlice(cnst.IdxFileHashLookupNamespace, r.rec.FileHash), db)
			if err != nil || !containsKey(ids, dbio.CanonicalIndexedKey(r.id)) {
				t.Errorf("%s: hash lookup %v, %v", path, ids, err)
			}
		}
	}
}

func containsKey(keys [][]byte, k []byte) bool {
	for _, x := range keys {
		if bytes.Equal(x, k) {
			return true
		}
	}
	return false
}

func countPrefix(t *testing.T, db *badger.DB, prefix []byte) int {
	t.Helper()
	n := 0
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestIndexerExtentPages: a file with more extents than a record carries
// keeps them in pages of their own.
func TestIndexerExtentPages(t *testing.T) {
	img := memImage(randomBytes(1<<20, 4))
	db := openIndexDB(t, img.Size())
	ix := newIndexer(t.Context(), Options{DB: db, EvidenceID: testEID, EvidenceName: "ev.img", Image: img, Workers: 2})
	n := structs.MaxRecordExtents + 10
	ext := make([]core.Extent, n)
	var want []byte
	for i := range ext {
		off := int64((i * 7) % (len(img) - 8))
		ext[i] = core.Extent{FileOffset: int64(i) * 8, Length: 8, ImageOffset: off}
		want = append(want, img[off:off+8]...)
	}
	part := core.PartitionInfo{Partition: core.Partition{Index: 1, Offset: 0, Size: img.Size(), Allocated: true}, FSType: core.FSNTFS, Status: core.PartIndexed}
	rec := core.FileRecord{Path: "/frag.bin", Kind: core.KindFile, Size: int64(len(want)),
		Content: core.Content{Kind: core.ContentExtents, Size: int64(len(want)), Located: int64(len(want)), DataBytes: int64(len(want)), Extents: ext}}
	if err := ix.beginPartition(part); err != nil {
		t.Fatal(err)
	}
	if err := ix.file(part, rec); err != nil {
		t.Fatal(err)
	}
	if err := ix.endPartition(part, &core.IndexReport{}); err != nil {
		t.Fatal(err)
	}
	if err := ix.stop(); err != nil {
		t.Fatal(err)
	}
	got := recordsOf(t, db, testEID)["/frag.bin"]
	if len(got) != 1 || got[0].rec.Content.ExtentPages != 2 || len(got[0].rec.Content.Extents) != 0 {
		t.Fatalf("record %+v, want 2 extent pages", got)
	}
	if b := contentOf(t, db, img, got[0].id, got[0].rec); !bytes.Equal(b, want) {
		t.Fatal("content read through extent pages differs")
	}
	// Deleting the index removes the pages too.
	if err := store.DeleteEvidenceIndex(t.Context(), db, testEID, nil); err != nil {
		t.Fatal(err)
	}
	if n := countPrefix(t, db, dbio.ExtentPagePrefix(got[0].id)); n != 0 {
		t.Errorf("%d extent pages left after delete", n)
	}
}

// TestInterruptedPassIsDeletable: a pass that stops part way (a crash, a
// cancel) leaves members its partition record does not count yet, records,
// and stored objects; deleting the evidence's index finds them all.
func TestInterruptedPassIsDeletable(t *testing.T) {
	img := memImage(randomBytes(64<<10, 5))
	db := openIndexDB(t, img.Size())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ix := newIndexer(ctx, Options{DB: db, EvidenceID: testEID, EvidenceName: "ev.img", Image: img, Workers: 2})
	part := core.PartitionInfo{Partition: core.Partition{Index: 0, Offset: 0, Size: img.Size(), Allocated: true}, FSType: core.FSNTFS, Status: core.PartIndexed}
	if err := ix.beginPartition(part); err != nil {
		t.Fatal(err)
	}
	var ids [][]byte
	for i, path := range []string{"/a.bin", "/b.bin", "/c.txt"} {
		rec := core.FileRecord{Path: path, Kind: core.KindFile, Size: 4096, Inode: uint64(i + 1),
			Content: core.Content{Kind: core.ContentExtents, Size: 4096, Located: 4096, DataBytes: 4096, Extents: []core.Extent{{FileOffset: 0, Length: 4096, ImageOffset: int64(i) * 4096}}}}
		if i == 2 {
			rec.Content = core.Content{Kind: core.ContentDecoded, Size: 4096, Located: 4096}
			rec.Open = opener(io.NopCloser(bytes.NewReader(img[:4096])))
		}
		ids = append(ids, IndexedID(testEID, 0, 0, rec.Inode, path, false, 0))
		if err := ix.file(part, rec); err != nil {
			t.Fatal(err)
		}
	}
	// The process dies before the partition ends: what the partition's
	// batch held is lost, and only the decoded file's member key (written
	// before its object) and the object's relations are on disk.
	cancel()
	ix.part.pending.Wait()
	ix.part.batch.Cancel()
	ix.part = nil
	if err := ix.stop(); err != nil {
		t.Fatal(err)
	}
	if n := countPrefix(t, db, dbio.CanonicalIndexedKey(ids[2])); n != 0 {
		t.Fatal("the decoded file's record was written although its batch was lost")
	}
	if n := countPrefix(t, db, dbio.PartitionMemberKey(ix.parts[0].ID, base64.StdEncoding.EncodeToString(ids[2]))); n != 1 {
		t.Fatal("the decoded file's member key is not on disk")
	}
	prec, err := dbio.GetPartitionFile(ix.parts[0].ID, db)
	if err != nil || prec.MemberCount != 0 {
		t.Fatalf("partition record before delete: members %d, %v", prec.MemberCount, err)
	}
	if n := countPrefix(t, db, dbio.RelationPrefix(ids[2])); n == 0 {
		t.Fatal("the decoded file's object was not stored")
	}

	if err := store.DeleteEvidenceIndex(t.Context(), db, testEID, nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		for _, k := range [][]byte{dbio.CanonicalIndexedKey(id), dbio.ObjectKey(id), dbio.RelationPrefix(id)} {
			if n := countPrefix(t, db, k); n != 0 {
				t.Errorf("%q: %d keys left after delete", k, n)
			}
		}
	}
	for _, k := range [][]byte{dbio.PartitionMemberPrefix(ix.parts[0].ID), dbio.CanonicalPartitionKey(ix.parts[0].ID)} {
		if n := countPrefix(t, db, k); n != 0 {
			t.Errorf("%q: %d keys left after delete", k, n)
		}
	}
}

func TestSetPartitionHash(t *testing.T) {
	img := memImage(make([]byte, 4096))
	db := openIndexDB(t, img.Size())
	ix := newIndexer(t.Context(), Options{DB: db, EvidenceID: testEID, EvidenceName: "ev.img", Image: img})
	part := core.PartitionInfo{Partition: core.Partition{Index: 2, Offset: 512, Size: 1024, Allocated: true}, Status: core.PartUnknown}
	if err := ix.beginPartition(part); err != nil {
		t.Fatal(err)
	}
	if err := ix.endPartition(part, &core.IndexReport{}); err != nil {
		t.Fatal(err)
	}
	if err := ix.stop(); err != nil {
		t.Fatal(err)
	}
	pid := ix.parts[0].ID
	members := func(hash []byte) [][]byte {
		lk := util.AppendToBytesSlice(cnst.PartiFileHashLookupNamespace, base64.StdEncoding.EncodeToString(hash))
		ids, err := dbio.GetHashLookupUUIDsByKey(lk, db)
		if err != nil && !errors.Is(err, badger.ErrKeyNotFound) {
			t.Fatal(err)
		}
		return ids
	}
	for _, h := range [][]byte{[]byte("first-hash"), []byte("second-hash")} {
		if err := SetPartitionHash(db, pid, h); err != nil {
			t.Fatal(err)
		}
		rec, err := dbio.GetPartitionFile(pid, db)
		if err != nil || rec.FileHash != base64.StdEncoding.EncodeToString(h) {
			t.Fatalf("record hash %q, %v", rec.FileHash, err)
		}
		if !containsKey(members(h), dbio.CanonicalPartitionKey(pid)) {
			t.Fatalf("lookup of %q does not name the partition", h)
		}
	}
	if len(members([]byte("first-hash"))) != 0 {
		t.Error("the replaced hash still names the partition")
	}
	// An unknown partition is still a record, named by its slot.
	rec, _ := dbio.GetPartitionFile(pid, db)
	if rec.Partition == nil || rec.Partition.Status != string(core.PartUnknown) || rec.Path != "ev.img_p2" || dbio.RecordPath(rec.IndexedFile) != "ev.img_p2" {
		t.Errorf("unknown partition record: %+v %+v", rec.IndexedFile, rec.Partition)
	}
	if ev, err := dbio.RecordEvidenceID(rec.IndexedFile); err != nil || !bytes.Equal(ev, testEID) {
		t.Errorf("partition evidence %x, %v", ev, err)
	}
}
