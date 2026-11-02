package near

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"indicer/internal/progress"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"

	"indicer/internal/safe"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/store"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// occurrence is a chunk at an offset of an evidence or materialised object.
type occurrence struct {
	owner  string
	offset int64
}

// queryChunk is one distinct chunk of the query.
type queryChunk struct {
	hash    []byte
	offsets []int64 // where it occurs in the query
	sig     uint64
	hasSig  bool
	matched atomic.Bool
}

// query is the chunks of a near search's query.
type query struct {
	chunks      []*queryChunk // distinct, in the order first seen
	byHash      map[string]*queryChunk
	occurrences int
	unstored    int
	unsigned    int
	// own are the stored bytes of the query itself, which are not matches.
	own map[occurrence]struct{}
}

func newQuery() *query {
	return &query{byHash: map[string]*queryChunk{}, own: map[occurrence]struct{}{}}
}

func (q *query) add(hash []byte, at int64) *queryChunk {
	q.occurrences++
	qc := q.byHash[string(hash)]
	if qc == nil {
		qc = &queryChunk{hash: append([]byte(nil), hash...)}
		q.byHash[string(hash)] = qc
		q.chunks = append(q.chunks, qc)
	}
	qc.offsets = append(qc.offsets, at)
	return qc
}

// storedQuery is the chunks of the stored object fid, read from its spans:
// each distinct chunk once (SRC-5), with every place it occurs.
func storedQuery(ctx context.Context, db *badger.DB, fid []byte, deep bool) (q *query, err error) {
	l, err := dbio.OpenLogical(db, fid, dbio.ReadOptions{})
	if err != nil {
		return nil, err
	}
	ph := progress.Start(ctx, "read query", progress.Bytes, l.Size())
	defer func() { ph.Done(err) }()
	owner := string(l.Evidence().ID())
	q = newQuery()
	err = l.Spans(ctx, dbio.SpanBatch(l.ChunkSize()), func(spans []dbio.ChunkSpan) error {
		for _, s := range spans {
			ph.Add(s.Len())
			if !s.Stored() {
				q.unstored++
				continue
			}
			q.add(s.Hash, s.Offset)
			q.own[occurrence{owner, s.Chunk}] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if deep {
		for _, qc := range q.chunks {
			sig, err := dbio.GetChonkSignature(qc.hash, db)
			switch {
			case err == nil:
				qc.sig, qc.hasSig = sig, true
			case errors.Is(err, badger.ErrKeyNotFound):
				q.unsigned++
			default:
				return nil, err
			}
		}
	}
	return q, nil
}

// fileQuery is the chunks of a file outside the database, cut and hashed as
// the database stores content, read in order without mapping the file
// (SRC-4). A file whose size is a multiple of the chunk size has no empty
// last chunk (SRC-11). It also returns the file's full-file SimHash.
func fileQuery(ctx context.Context, db *badger.DB, f io.ReaderAt, size int64, deep bool) (q *query, sig uint64, err error) {
	cs := dbmeta.ChunkSize(db)
	if cs <= 0 {
		return nil, 0, fmt.Errorf("the database has no chunk size")
	}
	ph := progress.Start(ctx, "read query", progress.Bytes, size)
	defer func() { ph.Done(err) }()
	newHash := dbmeta.ChunkHasher(db)
	q = newQuery()
	acc := newFileSimHashAccumulator()
	buf := make([]byte, cs)
	for off := int64(0); off < size; off += cs {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		n := int(min(cs, size-off))
		if _, err := f.ReadAt(buf[:n], off); err != nil && !(errors.Is(err, io.EOF) && n > 0) {
			return nil, 0, fmt.Errorf("read at %d: %w", off, err)
		}
		h := newHash()
		h.Write(buf[:n])
		qc := q.add(h.Sum(nil), off)
		if deep && !qc.hasSig {
			qc.sig, qc.hasSig = util.ChunkSimHash64(buf[:n]), true
		}
		acc.write(buf[:n])
		ph.Add(int64(n))
	}
	return q, acc.finalize(), nil
}

// Out finds the stored objects that share content with the file at path,
// outside the database, and writes the report. A file stored whole is
// reported as its exact matches, unless opts.ExplainExact.
func Out(ctx context.Context, db *badger.DB, path string, opts Options) (Outcome, error) {
	started := now()
	out := Outcome{ReportPath: reportPath(opts)}
	if err := checkReportPath(out.ReportPath, opts.Force); err != nil {
		return out, err
	}
	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return out, err
	}
	if !info.Mode().IsRegular() {
		return out, fmt.Errorf("%s is not a regular file", path)
	}
	size := info.Size()

	hashing := progress.Start(ctx, "hash file", progress.Bytes, size)
	sums, err := store.HashContent(progress.Within(ctx, hashing), db, f, size, false)
	hashing.Done(err)
	if err != nil {
		return out, err
	}
	enc := base64.StdEncoding.EncodeToString
	r := newRun(db, opts, started)
	exact, err := exactMatches(db, size, enc(sums.Plain), enc(sums.Root))
	if err != nil {
		return out, err
	}
	for _, id := range exact {
		r.addExact(id)
	}
	input := nearReportInput{
		QueryHashBase64: enc(sums.Plain),
		QueryID:         enc(sums.Plain),
		QueryType:       "external",
		QueryNames:      []string{filepath.Base(path)},
		ExactFileMatch:  len(exact) > 0,
		ExplainExact:    opts.ExplainExact,
	}
	if len(exact) > 0 && !opts.ExplainExact {
		out.Notes = append(out.Notes, "the file is stored whole; its chunks were not matched (pass --explain-exact to match them)")
		return r.finish(ctx, out, input, verifyConfig{})
	}
	if err := r.checkCapability(ctx); err != nil {
		return out, err
	}
	q, sig, err := fileQuery(ctx, db, f, size, opts.Deep)
	if err != nil {
		return out, err
	}
	if err := r.matchQuery(ctx, q); err != nil {
		return out, err
	}
	vcfg := verifyConfig{enabled: opts.Verify, topK: opts.TopK, querySig: sig, isOutfile: true}
	return r.finish(ctx, out, input, vcfg)
}

// exactMatches returns the stored objects whose whole content is the file's:
// records whose hash is the plain hash, and the evidence holding this content
// whatever hash its strategy recorded.
func exactMatches(db *badger.DB, size int64, plain, root string) ([][]byte, error) {
	ids, err := dbio.GuessFileTypes(plain, db)
	if err != nil && !errors.Is(err, badger.ErrKeyNotFound) {
		return nil, err
	}
	w, err := store.StoredByIdentity(db, size, root, plain)
	if err != nil {
		return nil, err
	}
	if w != nil {
		key := dbio.CanonicalEvidenceKey(w)
		dup := false
		for _, id := range ids {
			dup = dup || string(id) == string(key)
		}
		if !dup {
			ids = append(ids, key)
		}
	}
	return ids, nil
}

// deepPass matches, by SimHash, the query's signed chunks no stored chunk
// equals: one pass over the stored signatures finds each one's closest
// stored chunk, whose occurrences then count with the similarity as their
// confidence. Before, every such chunk read every relation in the database.
func (r *run) deepPass(ctx context.Context, q *query) (err error) {
	var todo []*queryChunk
	for _, qc := range q.chunks {
		if !qc.matched.Load() && qc.hasSig {
			todo = append(todo, qc)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	ph := progress.Start(ctx, "deep", progress.Unit("signatures"), progress.Unknown)
	defer func() { ph.Done(err) }()
	best := make([]float64, len(todo))
	bestHash := make([][]byte, len(todo))
	prefix := []byte(cnst.ChonkSimhashNamespace)
	err = r.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()
		n := 0
		for it.Rewind(); it.Valid(); it.Next() {
			if n++; n%4096 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
				ph.Add(4096)
			}
			data, err := it.Item().ValueCopy(nil)
			if err != nil {
				return err
			}
			if decoded, err := cnst.DECODER.DecodeAll(data, nil); err == nil {
				data = decoded
			}
			if len(data) != 8 {
				continue
			}
			sig := binary.BigEndian.Uint64(data)
			chash := it.Item().Key()[len(prefix):]
			for i, qc := range todo {
				sim := hammingSimilarity64(qc.sig, sig)
				if sim >= minDeepConfidence && sim > best[i] && string(chash) != string(qc.hash) {
					best[i], bestHash[i] = sim, append([]byte(nil), chash...)
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	g, gctx := safe.WithContext(ctx)
	g.SetLimit(r.workers)
	for i, qc := range todo {
		if bestHash[i] == nil {
			continue
		}
		g.Go(safe.Site{Component: "near", Op: "deep"}, func() error {
			found, err := r.matchChunk(gctx, q, bestHash[i], best[i], methodDeep, qc.offsets)
			if found {
				qc.matched.Store(true)
			}
			return err
		})
	}
	return g.Wait()
}

// storedSimhash is the full-file SimHash of the stored object fid, from the
// cache a previous phase 2 left, or computed from its content.
func storedSimhash(ctx context.Context, db *badger.DB, fid []byte) (uint64, bool, error) {
	if sig, err := dbio.GetFileSimhash(fid, db); err == nil {
		return sig, true, nil
	}
	l, err := dbio.OpenLogical(db, fid, dbio.ReadOptions{})
	if errors.Is(err, dbio.ErrContentUnavailable) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	acc := newFileSimHashAccumulator()
	if err := l.Stream(ctx, func(b []byte) error { acc.write(b); return nil }); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, false, ctxErr
		}
		return 0, false, nil
	}
	sig := acc.finalize()
	cacheSimhash(db, fid, sig)
	return sig, true, nil
}

// cacheSimhash keeps a full-file SimHash for later runs, when the database
// is writable; the cache is best-effort.
func cacheSimhash(db *badger.DB, fid []byte, sig uint64) {
	if !db.Opts().ReadOnly {
		_ = dbio.SetFileSimhash(fid, sig, db)
	}
}
