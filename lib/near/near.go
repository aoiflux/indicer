// Package near finds the stored objects that share content with a query: a
// stored object (near in) or a file outside the database (near out). Each of
// the query's chunks is looked up in the reverse relations, which record
// where every stored chunk occurs, and each occurrence is attributed to the
// most specific object holding it: an indexed file, else a partition, else
// the evidence. Deep mode also matches chunks whose SimHash signatures are
// close.
package near

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"

	"indicer/internal/safe"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/store"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

const (
	// MaxOccurrences bounds the occurrences looked at for one chunk. A
	// chunk stored in more places (zeros, a common header) says little
	// about which objects are related, and is skipped and counted.
	MaxOccurrences = 10000
	// minDeepConfidence is the least signature similarity deep mode counts.
	minDeepConfidence = 0.75
	// maxListedChunks bounds the matching chunks a report lists per match.
	maxListedChunks = 1000
	// DefaultReportPath is where a report goes when no path is given.
	DefaultReportPath = "near_report.json"
)

// maxOccurrences is MaxOccurrences; tests lower it.
var maxOccurrences = MaxOccurrences

var (
	// ErrNoReverseRelations means no stored evidence records where its
	// chunks occur, so nothing can be found (SRC-12).
	ErrNoReverseRelations = errors.New("no stored evidence has reverse relations")
	// ErrNoStoredEvidence means the database holds no stored evidence.
	ErrNoStoredEvidence = errors.New("the database holds no stored evidence")
	// ErrReportExists is a report path that exists, without Force.
	ErrReportExists = errors.New("report file already exists")
)

// Methods a chunk match is found by.
const (
	methodExact     = "shallow-exact"
	methodDeep      = "deep-simhash"
	methodExactFile = "exact-file-hash"
)

// Options control a near search.
type Options struct {
	// Deep also matches chunks no stored chunk equals, by SimHash.
	Deep bool
	// Verify re-ranks the TopK best matches by full-file SimHash (TopK 0
	// chooses from the memory and threads available).
	Verify bool
	TopK   int
	// ExplainExact matches the chunks of a file outside the database even
	// when the file is stored whole.
	ExplainExact bool
	// Out is the report path (DefaultReportPath when empty); an existing
	// file is replaced only with Force.
	Out   string
	Force bool
	// Workers bounds the chunks looked up at once; 0 means the thread count.
	Workers int
}

// Coverage says what a near search could and could not look at.
type Coverage struct {
	// QueryChunks are the chunk positions of the query; DistinctChunks the
	// different chunks among them; MatchedChunks those found elsewhere.
	QueryChunks    int `json:"query_chunks"`
	DistinctChunks int `json:"distinct_chunks"`
	MatchedChunks  int `json:"matched_chunks"`
	// UnstoredSpans are parts of a stored query held by no chunk (holes,
	// inline bytes), which cannot be matched.
	UnstoredSpans int `json:"unstored_spans,omitempty"`
	// CommonChunks were skipped: each occurs in more than MaxOccurrences
	// places.
	CommonChunks int `json:"common_chunks_skipped,omitempty"`
	// StaleOccurrences are occurrences in objects that are not stored, or
	// no longer exist.
	StaleOccurrences int64 `json:"stale_occurrences,omitempty"`
	// StoredEvidence counts the evidence that could hold matches;
	// WithoutReverseRelations those whose chunks cannot be found, because
	// they were stored without reverse relations.
	StoredEvidence          int `json:"stored_evidence"`
	WithoutReverseRelations int `json:"evidence_without_reverse_relations,omitempty"`
	// Deep mode: query chunks without a signature, and stored evidence
	// whose chunks have none, which deep mode cannot match.
	UnsignedQueryChunks int `json:"unsigned_query_chunks,omitempty"`
	WithoutSignatures   int `json:"evidence_without_signatures,omitempty"`
}

// Outcome is what a near search found and where its report went.
type Outcome struct {
	ReportPath string
	Matches    int
	Coverage   Coverage
	// Notes are what the user should know about how the search ran.
	Notes []string
}

// run is one near search's state: everything a search used to keep in
// package globals, reset (and raced on) by every call (SRC-6).
type run struct {
	db      *badger.DB
	opts    Options
	workers int

	mu      sync.Mutex
	matches map[string]*match
	exact   map[string]struct{}
	owners  map[string]*ownerInfo
	cov     Coverage
	stale   atomic.Int64
}

func newRun(db *badger.DB, opts Options) *run {
	workers := opts.Workers
	if workers <= 0 {
		workers = max(cnst.GetMaxThreadCount(), 1)
	}
	return &run{db: db, opts: opts, workers: workers, matches: map[string]*match{}, exact: map[string]struct{}{}, owners: map[string]*ownerInfo{}}
}

// match is one object the query shares content with.
type match struct {
	key    []byte
	target target
	// best is the best confidence at each matched chunk position of the
	// object (its evidence's offsets, or a materialised object's own).
	best   map[int64]float64
	method map[int64]string
	listed []nearChunkMatch
}

func (r *run) add(t target, conf float64, method string, queryOffsets []int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.matches[string(t.key)]
	if m == nil {
		m = &match{key: t.key, target: t, best: map[int64]float64{}, method: map[int64]string{}}
		r.matches[string(t.key)] = m
	}
	if conf > m.best[t.pos] {
		m.best[t.pos], m.method[t.pos] = conf, method
	}
	for _, off := range queryOffsets {
		if len(m.listed) >= maxListedChunks {
			break
		}
		m.listed = append(m.listed, nearChunkMatch{Index: off, Method: method, Confidence: conf, ConfidencePercent: conf * 100})
	}
}

// checkCapability counts the stored evidence and what near can find in it,
// and refuses a search that can find nothing.
func (r *run) checkCapability(ctx context.Context) error {
	err := forEachStoredEvidence(ctx, r.db, func(eid []byte, e structs.EvidenceFile) error {
		r.cov.StoredEvidence++
		ok, err := store.HasReverseRelations(r.db, eid, e)
		if err != nil {
			return err
		}
		if !ok {
			r.cov.WithoutReverseRelations++
		}
		if r.opts.Deep {
			ok, err := store.HasSignatures(r.db, eid, e)
			if err != nil {
				return err
			}
			if !ok {
				r.cov.WithoutSignatures++
			}
		}
		return nil
	})
	switch {
	case err != nil:
		return err
	case r.cov.StoredEvidence == 0:
		return ErrNoStoredEvidence
	case r.cov.WithoutReverseRelations == r.cov.StoredEvidence:
		return fmt.Errorf("%w: near needs them to find where chunks occur, and none of the %d stored evidence was stored with --revrel; add them with `dues repair --fix --revrel`",
			ErrNoReverseRelations, r.cov.StoredEvidence)
	}
	return nil
}

// notes are the coverage warnings a user should see.
func (r *run) notes() []string {
	var n []string
	c := r.cov
	if c.WithoutReverseRelations > 0 {
		n = append(n, fmt.Sprintf("%d of %d stored evidence have no reverse relations, so matches inside them cannot be found; add them with `dues repair --fix --revrel`", c.WithoutReverseRelations, c.StoredEvidence))
	}
	if c.CommonChunks > 0 {
		n = append(n, fmt.Sprintf("%d of the query's chunks occur in more than %d places each and were skipped", c.CommonChunks, maxOccurrences))
	}
	if r.opts.Deep && (c.UnsignedQueryChunks > 0 || c.WithoutSignatures > 0) {
		n = append(n, fmt.Sprintf("deep mode compares chunk signatures: %d query chunks and %d stored evidence have none (store with --simhash), so only exact chunk matches are reported for them", c.UnsignedQueryChunks, c.WithoutSignatures))
	}
	return n
}

func forEachStoredEvidence(ctx context.Context, db *badger.DB, fn func(eid []byte, e structs.EvidenceFile) error) error {
	type entry struct {
		id  []byte
		rec structs.EvidenceFile
	}
	var all []entry
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
			if storedEvidence(e) {
				all = append(all, entry{dbio.EvidenceRawID(it.Item().KeyCopy(nil)), e})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, e := range all {
		if err := fn(e.id, e.rec); err != nil {
			return err
		}
	}
	return nil
}

func storedEvidence(e structs.EvidenceFile) bool {
	return e.Completed && e.IngestState != structs.IngestStateDuplicate
}

// matchQuery looks up every distinct chunk of q, then, in deep mode, the
// chunks found nowhere by their signatures.
func (r *run) matchQuery(ctx context.Context, q *query) error {
	r.cov.QueryChunks, r.cov.DistinctChunks, r.cov.UnstoredSpans = q.occurrences, len(q.chunks), q.unstored
	g, gctx := safe.WithContext(ctx)
	g.SetLimit(r.workers)
	for _, qc := range q.chunks {
		g.Go(safe.Site{Component: "near", Op: "chunk"}, func() error {
			found, err := r.matchChunk(gctx, q, qc.hash, 1, methodExact, qc.offsets)
			if found {
				qc.matched.Store(true)
			}
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	if r.opts.Deep {
		if err := r.deepPass(ctx, q); err != nil {
			return err
		}
	}
	for _, qc := range q.chunks {
		if qc.matched.Load() {
			r.cov.MatchedChunks++
		}
	}
	r.cov.StaleOccurrences = r.stale.Load()
	return nil
}

// matchChunk attributes every occurrence of the stored chunk chash, other
// than the query's own bytes, with confidence conf. It reports whether any
// occurrence lies outside the query.
func (r *run) matchChunk(ctx context.Context, q *query, chash []byte, conf float64, method string, queryOffsets []int64) (bool, error) {
	var occs []occurrence
	truncated, err := dbio.ForEachChunkOccurrence(r.db, chash, maxOccurrences, func(owner []byte, off int64) error {
		occs = append(occs, occurrence{string(owner), off})
		return nil
	})
	if err != nil {
		return false, err
	}
	if truncated {
		r.mu.Lock()
		r.cov.CommonChunks++
		r.mu.Unlock()
		return false, nil
	}
	found := false
	for _, o := range occs {
		if err := ctx.Err(); err != nil {
			return found, err
		}
		if _, own := q.own[o]; own {
			continue
		}
		targets, err := r.attribute(ctx, o)
		if err != nil {
			return found, err
		}
		for _, t := range targets {
			found = true
			r.add(t, conf, method, queryOffsets)
		}
	}
	return found, nil
}

// addExact records an object whose whole content has the query's hash.
func (r *run) addExact(key []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exact[string(key)] = struct{}{}
}

// checkReportPath refuses an existing report path without force.
func checkReportPath(path string, force bool) error {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case info.IsDir():
		return fmt.Errorf("%w: %s is a directory", ErrReportExists, path)
	case !force:
		return fmt.Errorf("%w: %s; pass --force to replace it, or --out to write elsewhere", ErrReportExists, path)
	}
	return nil
}

func reportPath(opts Options) string {
	if opts.Out == "" {
		return DefaultReportPath
	}
	return opts.Out
}

// In finds the stored objects that share content with the stored object
// whose hash (base64) is hash, and writes the report.
func In(ctx context.Context, db *badger.DB, hash string, opts Options) (Outcome, error) {
	out := Outcome{ReportPath: reportPath(opts)}
	if err := checkReportPath(out.ReportPath, opts.Force); err != nil {
		return out, err
	}
	fids, err := dbio.GuessFileTypes(hash, db)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return out, fmt.Errorf("no stored object has hash %s: %w", hash, err)
	}
	if err != nil {
		return out, err
	}
	r := newRun(db, opts)
	if err := r.checkCapability(ctx); err != nil {
		return out, err
	}
	fid := fids[len(fids)-1]
	for _, id := range fids {
		r.addExact(id)
	}
	q, err := storedQuery(ctx, db, fid, opts.Deep)
	if err != nil {
		return out, err
	}
	r.cov.UnsignedQueryChunks = q.unsigned
	if err := r.matchQuery(ctx, q); err != nil {
		return out, err
	}

	var vcfg verifyConfig
	if opts.Verify {
		sig, ok, err := storedSimhash(ctx, db, fid)
		if err != nil {
			return out, err
		}
		if ok {
			vcfg = verifyConfig{enabled: true, topK: opts.TopK, querySig: sig}
		} else {
			out.Notes = append(out.Notes, "the query could not be read for its full-file SimHash; phase 2 was skipped")
		}
	}
	input := nearReportInput{
		QueryHashBase64: hash,
		QueryID:         base64.StdEncoding.EncodeToString(fid),
		QueryType:       nearFileType(fid),
		QueryNames:      recordNames(db, fid),
	}
	return r.finish(ctx, out, input, vcfg)
}
