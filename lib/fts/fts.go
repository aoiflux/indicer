// Package fts keeps the optional full-text index (--fts): a bleve index
// beside the database with, for each indexed file, its name and path and the
// text of its first bytes. Full-text mode uses it to choose the files a
// search reads; the counts always come from reading the files themselves.
package fts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/util"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search/query"
	"github.com/dgraph-io/badger/v4"
)

const (
	// DefaultLimit is the most candidates a search asks for by default.
	DefaultLimit = 5000
	// MaxTextBytes is how much of each file's content is read for its text.
	MaxTextBytes = 128 * 1024
	// maxTextRunes bounds the text kept for one file, in bytes of UTF-8.
	maxTextRunes = 80 * 1024
	// boltTimeout bounds the wait for an index another process holds.
	boltTimeout = "2s"
	// indexBatch is how many documents one bleve batch writes.
	indexBatch = 256
)

// ErrIndexNotReady means the database has no full-text index.
var ErrIndexNotReady = errors.New("full-text index not ready")

// textSuffix ends the ID of a file's text document in indexes written before
// W7, which kept a file's name and text as two documents.
const textSuffix = "#text"

type IndexedFileJob struct {
	FileID string // the indexed record's key
	Name   string // its path
}

type QueryMode int

const (
	QueryModeAnd QueryMode = iota
	QueryModeOr
)

func indexPath(db *badger.DB) string {
	return util.FTSPath(dbio.KVDir(db))
}

// handle is one open of a database's full-text index, shared by everyone in
// the process who uses it: bleve's store takes a file lock, so a second open
// of the same index in one process would wait on the first (SRC-7).
type handle struct {
	key  string
	idx  bleve.Index
	refs int
}

var (
	handlesMu sync.Mutex
	handles   = map[string]*handle{}
)

func handleKey(path string) string {
	key := filepath.Clean(path)
	if abs, err := filepath.Abs(key); err == nil {
		key = abs
	}
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return key
}

// acquire returns the database's full-text index, opened once and shared
// until the last release. A database opened read-only gets the index
// read-only. Without an index, a writable database with create gets a new
// one; otherwise acquire returns ErrIndexNotReady.
func acquire(db *badger.DB, create bool) (*handle, error) {
	path := indexPath(db)
	key := handleKey(path)
	handlesMu.Lock()
	defer handlesMu.Unlock()
	if h := handles[key]; h != nil {
		h.refs++
		return h, nil
	}
	readOnly := db.Opts().ReadOnly
	var idx bleve.Index
	_, err := os.Stat(path)
	switch {
	case err == nil:
		cfg := map[string]interface{}{"bolt_timeout": boltTimeout}
		if readOnly {
			cfg["read_only"] = true
		}
		if idx, err = bleve.OpenUsing(path, cfg); err != nil {
			return nil, fmt.Errorf("open full-text index %s (another process may hold it): %w", path, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	case !create || readOnly:
		return nil, ErrIndexNotReady
	default:
		if err := os.MkdirAll(filepath.Dir(path), cnst.DirPerm); err != nil {
			return nil, err
		}
		if idx, err = bleve.New(path, bleve.NewIndexMapping()); err != nil {
			return nil, fmt.Errorf("create full-text index %s: %w", path, err)
		}
	}
	h := &handle{key: key, idx: idx, refs: 1}
	handles[key] = h
	return h, nil
}

func (h *handle) release() error {
	handlesMu.Lock()
	defer handlesMu.Unlock()
	if h.refs--; h.refs > 0 {
		return nil
	}
	delete(handles, h.key)
	return h.idx.Close()
}

// IndexStats says what indexing wrote.
type IndexStats struct {
	Files    int // documents written
	WithText int // of which hold text from the file's content
	// TextErrors counts files whose content could not be read; their
	// documents hold the name only.
	TextErrors int
}

// TextError is files whose content could not be read for their text. Their
// names are indexed; the index covers them less than the others.
type TextError struct {
	Failed int
	First  error
}

func (e *TextError) Error() string {
	return fmt.Sprintf("the text of %d indexed files could not be read; first: %v", e.Failed, e.First)
}

func (e *TextError) Unwrap() error { return e.First }

// IndexFiles writes one document per job: the file's name and path, and the
// text of its first MaxTextBytes. Indexing goes on past a file whose content
// cannot be read, and then returns a *TextError counting them.
func IndexFiles(ctx context.Context, db *badger.DB, jobs []IndexedFileJob) (stats IndexStats, err error) {
	if len(jobs) == 0 {
		return stats, nil
	}
	h, err := acquire(db, true)
	if err != nil {
		return stats, err
	}
	defer func() {
		if cerr := h.release(); err == nil {
			err = cerr
		}
	}()
	var first error
	b := h.idx.NewBatch()
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		text, terr := extractText(ctx, db, []byte(job.FileID))
		if terr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return stats, ctxErr
			}
			stats.TextErrors++
			if first == nil {
				first = fmt.Errorf("%q: %w", job.Name, terr)
			}
		}
		if text != "" {
			stats.WithText++
		}
		// A document from an index written before W7 is replaced whole.
		b.Delete(job.FileID + textSuffix)
		if err := b.Index(job.FileID, document(job.Name, text)); err != nil {
			return stats, err
		}
		stats.Files++
		if b.Size() >= indexBatch {
			if err := h.idx.Batch(b); err != nil {
				return stats, err
			}
			b.Reset()
		}
	}
	if b.Size() > 0 {
		if err := h.idx.Batch(b); err != nil {
			return stats, err
		}
	}
	if stats.TextErrors > 0 {
		return stats, &TextError{Failed: stats.TextErrors, First: first}
	}
	return stats, nil
}

// document is a file's document: its name and path, split at separators so
// each part is a word, and the text of its content.
func document(name, text string) map[string]string {
	var b strings.Builder
	b.WriteString(name)
	for _, sep := range []string{"/", "_", "\\", "."} {
		b.WriteByte(' ')
		b.WriteString(strings.ReplaceAll(name, sep, " "))
	}
	return map[string]string{"name": b.String(), "content": text, "doc_type": "indexed"}
}

// DeleteIndexedFiles removes the documents of the indexed files fileIDs
// (their record keys). A database with no full-text index has nothing to
// delete.
func DeleteIndexedFiles(db *badger.DB, fileIDs []string) (err error) {
	if len(fileIDs) == 0 {
		return nil
	}
	h, err := acquire(db, false)
	if errors.Is(err, ErrIndexNotReady) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() {
		if cerr := h.release(); err == nil {
			err = cerr
		}
	}()
	const batchSize = 4096
	for i := 0; i < len(fileIDs); i += batchSize {
		b := h.idx.NewBatch()
		for _, id := range fileIDs[i:min(i+batchSize, len(fileIDs))] {
			b.Delete(id)
			b.Delete(id + textSuffix)
		}
		if err := h.idx.Batch(b); err != nil {
			return err
		}
	}
	return nil
}

// Candidates is what the full-text index offers for a query.
type Candidates struct {
	// Scores are by document ID: a record key, or, in an index written
	// before W7, a record key and textSuffix.
	Scores map[string]float64
	// Total is the documents the query matched, which can be more than
	// were returned.
	Total uint64
}

// IDs returns the record keys of the candidates.
func (c Candidates) IDs() map[string]struct{} {
	ids := make(map[string]struct{}, len(c.Scores))
	for id := range c.Scores {
		ids[strings.TrimSuffix(id, textSuffix)] = struct{}{}
	}
	return ids
}

// FileScores returns the best score of each candidate, by record key.
func (c Candidates) FileScores() map[string]float64 {
	out := make(map[string]float64, len(c.Scores))
	for id, s := range c.Scores {
		key := strings.TrimSuffix(id, textSuffix)
		out[key] = max(out[key], s)
	}
	return out
}

// Capped reports whether the index matched more documents than it returned.
func (c Candidates) Capped() bool { return c.Total > uint64(len(c.Scores)) }

// SearchCandidates returns up to limit documents that hold every term (or,
// with QueryModeOr, any term), each term matched as a phrase of the words it
// holds. The terms are never parsed as bleve query syntax (SRC-3).
func SearchCandidates(ctx context.Context, db *badger.DB, terms []string, mode QueryMode, limit int) (c Candidates, err error) {
	q := buildQuery(terms, mode)
	if q == nil {
		return Candidates{Scores: map[string]float64{}}, nil
	}
	h, err := acquire(db, false)
	if err != nil {
		return c, err
	}
	defer func() {
		if cerr := h.release(); err == nil {
			err = cerr
		}
	}()
	if limit <= 0 {
		limit = DefaultLimit
	}
	res, err := h.idx.SearchInContext(ctx, bleve.NewSearchRequestOptions(q, limit, 0, false))
	if err != nil {
		return c, err
	}
	c = Candidates{Scores: make(map[string]float64, len(res.Hits)), Total: res.Total}
	for _, hit := range res.Hits {
		c.Scores[hit.ID] = hit.Score
	}
	return c, nil
}

func buildQuery(terms []string, mode QueryMode) query.Query {
	var qs []query.Query
	for _, t := range terms {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			qs = append(qs, bleve.NewMatchPhraseQuery(t))
		}
	}
	switch {
	case len(qs) == 0:
		return nil
	case len(qs) == 1:
		return qs[0]
	case mode == QueryModeOr:
		return bleve.NewDisjunctionQuery(qs...)
	}
	return bleve.NewConjunctionQuery(qs...)
}

// errTextFull ends a file's text extraction once it has enough.
var errTextFull = errors.New("text full")

// extractText returns the text of the first MaxTextBytes of an indexed file:
// letters, digits and a few separators, lower-cased, from the parts that
// look like UTF-8 text. A file without content has no text.
func extractText(ctx context.Context, db *badger.DB, fid []byte) (string, error) {
	l, err := dbio.OpenLogical(db, fid, dbio.ReadOptions{})
	if errors.Is(err, dbio.ErrContentUnavailable) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	x := textExtractor{remaining: MaxTextBytes}
	err = l.Stream(ctx, func(b []byte) error {
		if x.write(b) {
			return errTextFull
		}
		return nil
	})
	if errors.Is(err, errTextFull) {
		err = nil
	}
	return strings.TrimSpace(x.out.String()), err
}

// textExtractor collects text from a file's bytes as they stream past. A
// rune cut by the end of one piece is completed from the next, so a piece is
// not taken for binary because a chunk boundary split a rune.
type textExtractor struct {
	remaining int
	carry     []byte
	out       strings.Builder
}

// write adds the text of b; it returns true once no more is wanted.
func (x *textExtractor) write(b []byte) bool {
	if x.remaining <= 0 || x.out.Len() >= maxTextRunes {
		return true
	}
	b = b[:min(len(b), x.remaining)]
	x.remaining -= len(b)
	data := append(x.carry, b...)
	// Hold back the start of a rune the next piece completes.
	cut := len(data)
	for i := len(data) - 1; i >= 0 && i >= len(data)-utf8.UTFMax+1; i-- {
		if utf8.RuneStart(data[i]) {
			if !utf8.FullRune(data[i:]) {
				cut = i
			}
			break
		}
	}
	body := data[:cut]
	x.carry = append([]byte(nil), data[cut:]...)
	if len(body) == 0 {
		return x.remaining <= 0
	}
	if !utf8.Valid(body) || !isLikelyText(body) {
		// Text on either side of a binary piece is not one word.
		x.out.WriteByte(' ')
		return x.remaining <= 0
	}
	for len(body) > 0 {
		r, size := utf8.DecodeRune(body)
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsSpace(r) || strings.ContainsRune("._-/:\\", r) {
			x.out.WriteRune(unicode.ToLower(r))
		} else {
			x.out.WriteByte(' ')
		}
		if x.out.Len() >= maxTextRunes {
			return true
		}
		body = body[size:]
	}
	return x.remaining <= 0
}

func isLikelyText(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	printable := 0
	for _, b := range data {
		if b == 9 || b == 10 || b == 13 || (b >= 32 && b <= 126) || b >= 128 {
			printable++
		}
	}
	return float64(printable)/float64(len(data)) >= 0.85
}
