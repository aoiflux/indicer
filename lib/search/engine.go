package search

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"indicer/internal/progress"
	"slices"
	"sync"

	"indicer/internal/faultinject"
	"indicer/internal/safe"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/engine"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// Level is the kind of object a hit is in.
type Level string

const (
	LevelEvidence  Level = "evidence"
	LevelPartition Level = "partition"
	LevelIndexed   Level = "indexed"
)

// Limits on a query.
const (
	// MaxTermBytes is the longest term a search accepts.
	MaxTermBytes = 1024
	// MaxTerms is the most terms one query may have.
	MaxTerms = 16
)

// ErrQueryTooLong is a query with a term longer than MaxTermBytes, or more
// than MaxTerms terms.
var ErrQueryTooLong = fmt.Errorf("query too long: at most %d terms of at most %d bytes", MaxTerms, MaxTermBytes)

// Options control a search.
type Options struct {
	// Workers bounds the objects read at once, and the chunks read at once
	// across them. Zero means the database's tuning's MaxThreads.
	Workers int
	// Candidates, when not nil, restricts the search to these indexed files
	// (record keys), as full-text mode does; evidence and partitions are not
	// read.
	Candidates map[string]struct{}
	// Progress, when set, counts each object read; its total becomes the
	// objects the search will read, and its "read" meter the bytes.
	Progress *progress.Phase
}

// Hit is an object whose bytes hold at least one of the terms.
type Hit struct {
	Key        []byte // record key
	Level      Level
	EvidenceID []byte  // raw ID of the object's evidence
	Size       int64   // bytes the object's reader serves
	Counts     []int64 // matches per term, in Result.Terms order
}

// Coverage says what a search read, and what it did not.
type Coverage struct {
	// Objects read, by level, and the bytes matched.
	Evidence, Partitions, Files int64
	Bytes                       int64
	// NotStored counts evidence records not searched because their content
	// is not stored: pending, failed, or a duplicate being removed.
	NotStored int64
	// Quarantined counts evidence whose index is quarantined; its partitions
	// and files were not searched.
	Quarantined int64
	// NotCandidates counts, in full-text mode, the objects the full-text
	// index did not offer: evidence, partitions and other indexed files.
	NotCandidates int64
	// Unavailable counts indexed files with no content to read: metadata
	// only, or a symlink.
	Unavailable int64
	// Unreadable counts objects whose read failed; Problems names the first
	// few, with their errors.
	Unreadable int64
	Problems   []string
}

// Incomplete reports whether some object could not be read.
func (c Coverage) Incomplete() bool { return c.Unreadable > 0 }

// Result is what a search found.
type Result struct {
	Terms    []string
	Hits     []Hit // sorted by key
	Coverage Coverage
}

// LevelTotal is the matches of term i, summed over the hits of one level.
// Summing over every level would count the same stored bytes once per level
// (SRC-9).
func (r *Result) LevelTotal(level Level, i int) int64 {
	var n int64
	for _, h := range r.Hits {
		if h.Level == level {
			n += h.Counts[i]
		}
	}
	return n
}

const maxProblems = 20

// validateTerms checks a query's terms.
func validateTerms(terms []string) error {
	if len(terms) == 0 {
		return cnst.ErrSmallQuery
	}
	if len(terms) > MaxTerms {
		return ErrQueryTooLong
	}
	for _, t := range terms {
		if len(t) < 2 {
			return cnst.ErrSmallQuery
		}
		if len(t) > MaxTermBytes {
			return ErrQueryTooLong
		}
	}
	return nil
}

// Run searches every stored object of db for terms: each COMPLETED
// evidence, then its partitions, then its indexed files, each read through
// the logical reader (I4), so a file's extents, holes, inline bytes and
// materialised content are searched as the file's own bytes.
func Run(ctx context.Context, db *badger.DB, terms []string, opts Options) (*Result, error) {
	if err := validateTerms(terms); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := newScanner(db, terms, opts)
	evidence, err := listEvidence(ctx, db)
	if err != nil {
		return nil, err
	}
	s.total = s.plannedObjects(evidence)
	s.progress.SetTotal(s.total)
	for _, ev := range evidence {
		if err := faultinject.Hit(faultinject.SearchMid); err != nil {
			return nil, err
		}
		if err := s.searchEvidence(ctx, ev); err != nil {
			return nil, err
		}
	}
	res := &Result{Terms: terms, Hits: s.hits, Coverage: s.cov}
	slices.SortFunc(res.Hits, func(a, b Hit) int { return bytes.Compare(a.Key, b.Key) })
	return res, nil
}

type evidenceEntry struct {
	id  []byte
	rec structs.EvidenceFile
}

// stored reports whether an evidence's content is stored and searchable.
func stored(e structs.EvidenceFile) bool {
	return e.Completed && e.IngestState != structs.IngestStateDuplicate
}

func listEvidence(ctx context.Context, db *badger.DB) ([]evidenceEntry, error) {
	var out []evidenceEntry
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(cnst.EviFileNamespace)
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			data, err := it.Item().ValueCopy(nil)
			if err != nil {
				return err
			}
			var e structs.EvidenceFile
			if err := dbio.DecodeRecord(data, &e); err != nil {
				return fmt.Errorf("evidence record %q: %w", it.Item().Key(), err)
			}
			out = append(out, evidenceEntry{id: dbio.EvidenceRawID(it.Item().KeyCopy(nil)), rec: e})
		}
		return nil
	})
	return out, err
}

// scanner holds one search's state.
type scanner struct {
	db         *badger.DB
	terms      []*matcher
	window     int // the largest window of the terms
	workers    int
	candidates map[string]struct{}
	progress   *progress.Phase
	read       *progress.Meter
	total      int64

	reads  chan struct{} // one token per chunk read in flight, across objects
	chunks *dbio.ChunkCache
	counts *countCache

	mu   sync.Mutex
	hits []Hit
	cov  Coverage
}

// readBudget bounds the decoded chunk bytes a search holds for matching.
const readBudget = 256 * cnst.MB

func newScanner(db *badger.DB, terms []string, opts Options) *scanner {
	workers := opts.Workers
	if workers <= 0 {
		workers = max(engine.For(db).Tuning().MaxThreads(), 1)
	}
	s := &scanner{
		db:         db,
		workers:    workers,
		candidates: opts.Candidates,
		progress:   opts.Progress,
		read:       opts.Progress.Meter("read", progress.Bytes),
		reads:      make(chan struct{}, workers),
		chunks:     dbio.NewChunkCache(dbio.DefaultChunkCacheBytes),
		counts:     newCountCache(64 * cnst.MB),
	}
	for _, t := range terms {
		m := newMatcher(t)
		s.terms = append(s.terms, m)
		s.window = max(s.window, m.window)
	}
	return s
}

// plannedObjects counts the objects the search will read, for progress.
func (s *scanner) plannedObjects(evidence []evidenceEntry) int64 {
	if s.candidates != nil {
		return int64(len(s.candidates))
	}
	var n int64
	for _, ev := range evidence {
		if !stored(ev.rec) {
			continue
		}
		n++
		if quarantined(ev.rec) {
			continue
		}
		for pidB64 := range ev.rec.InternalObjects {
			n++
			if pid, err := base64.StdEncoding.DecodeString(pidB64); err == nil {
				if prec, err := dbio.GetPartitionFile(pid, s.db); err == nil {
					n += int64(dbio.PartitionMemberCount(prec))
				}
			}
		}
	}
	return n
}

func quarantined(e structs.EvidenceFile) bool {
	return e.Index != nil && e.Index.State == structs.IndexInvalid
}

// object is one record a phase reads; start orders files by where they lie
// in the evidence.
type object struct {
	key   []byte
	start int64
}

// searchEvidence searches one evidence: the evidence itself first, so the
// partitions and files after it find most chunks counted already, then its
// partitions, then its files in evidence order, so neighbouring small files
// share decoded chunks.
func (s *scanner) searchEvidence(ctx context.Context, ev evidenceEntry) error {
	if !stored(ev.rec) {
		s.mu.Lock()
		s.cov.NotStored++
		s.mu.Unlock()
		return nil
	}
	full := s.candidates == nil
	if full {
		if err := s.phase(ctx, LevelEvidence, ev.id, []object{{key: dbio.CanonicalEvidenceKey(ev.id)}}, 1); err != nil {
			return err
		}
	}
	if quarantined(ev.rec) {
		s.mu.Lock()
		s.cov.Quarantined++
		s.mu.Unlock()
		return nil
	}

	var parts, files []object
	for pidB64 := range ev.rec.InternalObjects {
		pid, err := base64.StdEncoding.DecodeString(pidB64)
		if err != nil {
			s.problem([]byte(pidB64), fmt.Errorf("partition ID of evidence %x: %w", ev.id, err))
			continue
		}
		pkey := dbio.CanonicalPartitionKey(pid)
		prec, err := dbio.GetPartitionFile(pkey, s.db)
		if err != nil {
			s.problem(pkey, err)
			continue
		}
		parts = append(parts, object{key: pkey, start: prec.Start})
		err = dbio.ForEachPartitionMember(s.db, pkey, prec, func(iidB64 string, off structs.InternalOffset) error {
			iid, err := base64.StdEncoding.DecodeString(iidB64)
			if err != nil {
				s.problem([]byte(iidB64), fmt.Errorf("member of partition %x: %w", pid, err))
				return nil
			}
			key := dbio.CanonicalIndexedKey(iid)
			if !full {
				if _, ok := s.candidates[string(key)]; !ok {
					s.mu.Lock()
					s.cov.NotCandidates++
					s.mu.Unlock()
					return nil
				}
			}
			files = append(files, object{key: key, start: off.Start})
			return nil
		})
		if err != nil {
			return err
		}
	}
	slices.SortFunc(parts, compareObjects)
	slices.SortFunc(files, compareObjects)
	if full {
		if err := s.phase(ctx, LevelPartition, ev.id, parts, s.workers); err != nil {
			return err
		}
	} else {
		s.mu.Lock()
		s.cov.NotCandidates += 1 + int64(len(parts))
		s.mu.Unlock()
	}
	return s.phase(ctx, LevelIndexed, ev.id, files, s.workers)
}

func compareObjects(a, b object) int {
	if a.start != b.start {
		return compareInt64(a.start, b.start)
	}
	return bytes.Compare(a.key, b.key)
}

func compareInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// phase reads objects, at most parallel at a time.
func (s *scanner) phase(ctx context.Context, level Level, eid []byte, objects []object, parallel int) error {
	if len(objects) == 0 {
		return nil
	}
	g, gctx := safe.WithContext(ctx)
	g.SetLimit(parallel)
	for _, o := range objects {
		g.Go(safe.Site{Component: "search", Op: "scan", Path: string(o.key)}, func() error {
			err := s.scanObject(gctx, level, eid, o.key, parallel)
			s.progress.Add(1)
			return err
		})
	}
	return g.Wait()
}

// scanObject counts the terms in one object. An object that cannot be read
// is counted and named in the coverage; only a cancelled search is an error.
func (s *scanner) scanObject(ctx context.Context, level Level, eid, key []byte, parallel int) error {
	l, err := dbio.OpenLogical(s.db, key, dbio.ReadOptions{Cache: s.chunks})
	if errors.Is(err, dbio.ErrContentUnavailable) {
		s.mu.Lock()
		s.cov.Unavailable++
		s.mu.Unlock()
		return nil
	}
	if err != nil {
		s.problem(key, err)
		return nil
	}
	ts := make([]*termState, len(s.terms))
	for i, m := range s.terms {
		ts[i] = &termState{m: m}
	}
	cs := max(l.ChunkSize(), 1)
	batch := int(min(int64(dbio.SpanBatch(cs)), max(1, readBudget/(cs*int64(parallel)))))
	err = l.Spans(ctx, batch, func(spans []dbio.ChunkSpan) error { return s.feedSpans(ctx, l, spans, ts) })
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		s.problem(key, err)
		return nil
	}

	counts := make([]int64, len(ts))
	any := false
	for i, t := range ts {
		counts[i] = t.count
		any = any || t.count > 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch level {
	case LevelEvidence:
		s.cov.Evidence++
	case LevelPartition:
		s.cov.Partitions++
	default:
		s.cov.Files++
	}
	s.cov.Bytes += l.Size()
	if any {
		s.hits = append(s.hits, Hit{Key: key, Level: level, EvidenceID: eid, Size: l.Size(), Counts: counts})
	}
	return nil
}

func (s *scanner) problem(key []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cov.Unreadable++
	if len(s.cov.Problems) < maxProblems {
		s.cov.Problems = append(s.cov.Problems, fmt.Sprintf("%q: %v", key, err))
	}
}

// feedSpans feeds one batch of an object's spans to its terms, in order.
// Spans whose counts are cached are not read; the others are read in
// parallel, each read holding one of the search's read tokens.
func (s *scanner) feedSpans(ctx context.Context, l *dbio.Logical, spans []dbio.ChunkSpan, ts []*termState) error {
	type slot struct {
		data   []byte
		cached *spanCounts
		err    error
	}
	slots := make([]slot, len(spans))
	var wg sync.WaitGroup
	var acquireErr error
	for i, sp := range spans {
		if s.cacheable(sp) {
			if c := s.counts.get(sp); c != nil {
				slots[i].cached = c
				continue
			}
		}
		if !sp.Stored() {
			slots[i].data, slots[i].err = l.ReadSpan(sp)
			continue
		}
		select {
		case s.reads <- struct{}{}:
		case <-ctx.Done():
			acquireErr = ctx.Err()
		}
		if acquireErr != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-s.reads }()
			slots[i].err = safe.Call(safe.Site{Component: "search", Op: "read"}, func() error {
				var err error
				slots[i].data, err = l.ReadSpan(sp)
				return err
			})
		}()
	}
	wg.Wait()
	if acquireErr != nil {
		return acquireErr
	}
	for i, sp := range spans {
		sl := &slots[i]
		s.read.Add(sp.Len())
		if sl.cached != nil {
			if s.applyCached(ts, sl.cached) {
				continue
			}
			// Here a match runs into the span, so its scan does not start
			// at its first byte as the cached one did.
			sl.data, sl.err = l.ReadSpan(sp)
		}
		if sl.err != nil {
			return sl.err
		}
		s.feedData(ts, sp, sl.data)
	}
	return nil
}

// cacheable reports whether a span's counts may be cached: a stored chunk's
// span at least as long as every term's window, so the junction with the
// bytes before it can always be decided from its head.
func (s *scanner) cacheable(sp dbio.ChunkSpan) bool {
	return sp.Stored() && sp.Len() >= int64(s.window)
}

// feedData feeds a span's bytes to every term, and caches what the span's
// own scan found when every term's scan started at its first byte.
func (s *scanner) feedData(ts []*termState, sp dbio.ChunkSpan, data []byte) {
	var c *spanCounts
	if s.cacheable(sp) {
		c = &spanCounts{interior: make([]int64, len(ts)), next: make([]int, len(ts))}
	}
	for i, t := range ts {
		start, interior, next := t.feed(data)
		if c == nil {
			continue
		}
		if start != 0 {
			c = nil
			continue
		}
		c.interior[i], c.next[i] = interior, next
	}
	if c == nil {
		return
	}
	tail := min(len(data), s.window-1)
	c.head = append([]byte(nil), data[:s.window]...)
	c.tail = append([]byte(nil), data[len(data)-tail:]...)
	c.tailOff = len(data) - tail
	s.counts.put(sp, c)
}

// applyCached feeds a cached span to every term, when every term's scan of
// it starts at its first byte, as the cached scan did; otherwise it changes
// nothing and returns false.
func (s *scanner) applyCached(ts []*termState, c *spanCounts) bool {
	jn := make([]int64, len(ts))
	for i, t := range ts {
		n, start, _, ok := t.junction(c.head)
		if !ok || start != 0 {
			return false
		}
		jn[i] = n
	}
	for i, t := range ts {
		t.count += jn[i] + c.interior[i]
		t.carry = append(t.carry[:0], c.tail[c.next[i]-c.tailOff:]...)
	}
	return true
}

// spanCounts is what scanning one span from its first byte found: per term,
// the matches and where the scan stopped, and the span's first and last
// bytes, which the junctions with its neighbours need.
type spanCounts struct {
	head, tail []byte
	tailOff    int
	interior   []int64
	next       []int
}

type spanKey struct {
	hash   string
	lo, hi int64
}

// countCache keeps spanCounts by chunk hash and span, within a byte budget;
// once full, it keeps what it has.
type countCache struct {
	mu     sync.Mutex
	m      map[spanKey]*spanCounts
	used   int64
	budget int64
}

func newCountCache(budget int64) *countCache {
	return &countCache{m: map[spanKey]*spanCounts{}, budget: budget}
}

func (c *countCache) get(sp dbio.ChunkSpan) *spanCounts {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[spanKey{string(sp.Hash), sp.Lo, sp.Hi}]
}

func (c *countCache) put(sp dbio.ChunkSpan, v *spanCounts) {
	cost := int64(len(sp.Hash)+len(v.head)+len(v.tail)) + int64(len(v.interior))*16 + 96
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used+cost > c.budget {
		return
	}
	k := spanKey{string(sp.Hash), sp.Lo, sp.Hi}
	if _, ok := c.m[k]; !ok {
		c.m[k] = v
		c.used += cost
	}
}
