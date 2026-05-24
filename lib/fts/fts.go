package fts

import (
	"context"
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/util"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/blevesearch/bleve/v2"
	"github.com/dgraph-io/badger/v4"
	"github.com/schollz/progressbar/v3"
)

const (
	defaultLimit             = 5000
	defaultMaxIndexBytes     = 128 * 1024
	defaultMaxExtractedRunes = 80 * 1024
)

var ErrIndexNotReady = errors.New("full-text index not ready")

type IndexedFileJob struct {
	FileID string
	Name   string
}

type QueryMode int

const (
	QueryModeAnd QueryMode = iota
	QueryModeOr
)

func indexPath(db *badger.DB) string {
	return util.FTSPath(db.Opts().Dir)
}

func createOrOpen(db *badger.DB) (bleve.Index, error) {
	path := indexPath(db)
	if _, err := os.Stat(path); err == nil {
		return bleve.Open(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), cnst.DirPerm); err != nil {
		return nil, err
	}
	return bleve.New(path, bleve.NewIndexMapping())
}

func openExisting(db *badger.DB) (bleve.Index, error) {
	path := indexPath(db)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrIndexNotReady
		}
		return nil, err
	}
	return bleve.Open(path)
}

func IndexNames(db *badger.DB, fileID string, name string, docType string) error {
	if name == "" {
		return nil
	}
	index, err := createOrOpen(db)
	if err != nil {
		return err
	}
	defer index.Close()
	return indexNamesWithIndex(index, fileID, name, docType)
}

// IndexIndexedFile indexes both name/path metadata and a capped textual extract
// from logical file bytes for richer full-text candidate retrieval.
func IndexIndexedFile(db *badger.DB, fileID string, name string) error {
	index, err := createOrOpen(db)
	if err != nil {
		return err
	}
	defer index.Close()
	return indexIndexedFileWithIndex(index, db, fileID, name)
}

// IndexIndexedFileJobs indexes many indexed-file documents in a single index
// open/close cycle to avoid churn and noisy backend maintenance logs.
func IndexIndexedFileJobs(db *badger.DB, jobs []IndexedFileJob, onIndexed func()) error {
	if len(jobs) == 0 {
		return nil
	}

	index, err := createOrOpen(db)
	if err != nil {
		return err
	}
	defer index.Close()

	for _, job := range jobs {
		if err := indexIndexedFileWithIndex(index, db, job.FileID, job.Name); err != nil {
			return err
		}
		if onIndexed != nil {
			onIndexed()
		}
	}
	return nil
}

func indexIndexedFileWithIndex(index bleve.Index, db *badger.DB, fileID string, name string) error {
	if err := indexNamesWithIndex(index, fileID, name, "indexed"); err != nil {
		return err
	}
	text := extractTextForFTS(db, []byte(fileID), defaultMaxIndexBytes, defaultMaxExtractedRunes)
	if text == "" {
		return nil
	}
	return index.Index(fileID+"#text", map[string]string{
		"content":  text,
		"doc_type": "indexed_text",
	})
}

func SearchFileScores(ctx context.Context, db *badger.DB, query string, limit int) (map[string]float64, error) {
	index, err := openExisting(db)
	if err != nil {
		return nil, err
	}
	defer index.Close()

	if limit <= 0 {
		limit = defaultLimit
	}

	q := bleve.NewQueryStringQuery(query)
	req := bleve.NewSearchRequestOptions(q, limit, 0, false)
	res, err := index.SearchInContext(ctx, req)
	if err != nil {
		return nil, err
	}

	scores := make(map[string]float64, len(res.Hits))
	for _, hit := range res.Hits {
		scores[hit.ID] = hit.Score
	}
	return scores, nil
}

func IndexDocumentCount(db *badger.DB) (uint64, error) {
	index, err := openExisting(db)
	if err != nil {
		return 0, err
	}
	defer index.Close()

	return index.DocCount()
}

// SearchFileScoresParsed evaluates FTS scores using already-parsed terms and a
// caller-selected AND/OR mode so FTS candidate retrieval matches higher-level
// query semantics.
func SearchFileScoresParsed(ctx context.Context, db *badger.DB, terms []string, mode QueryMode, limit int) (map[string]float64, error) {
	index, err := openExisting(db)
	if err != nil {
		return nil, err
	}
	defer index.Close()

	if limit <= 0 {
		limit = defaultLimit
	}

	query := buildParsedQueryString(terms, mode)
	if query == "" {
		return map[string]float64{}, nil
	}

	q := bleve.NewQueryStringQuery(query)
	req := bleve.NewSearchRequestOptions(q, limit, 0, false)
	res, err := index.SearchInContext(ctx, req)
	if err != nil {
		return nil, err
	}

	scores := make(map[string]float64, len(res.Hits))
	for _, hit := range res.Hits {
		scores[hit.ID] = hit.Score
	}
	return scores, nil
}

func buildParsedQueryString(terms []string, mode QueryMode) string {
	parts := make([]string, 0, len(terms))
	for _, term := range terms {
		t := strings.TrimSpace(strings.ToLower(term))
		if t == "" {
			continue
		}
		parts = append(parts, ftsTermToQueryString(t))
	}
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return parts[0]
	}
	sep := " AND "
	if mode == QueryModeOr {
		sep = " OR "
	}
	return strings.Join(parts, sep)
}

func ftsTermToQueryString(term string) string {
	escaped := escapeQueryStringTerm(term)
	if strings.ContainsRune(term, ' ') {
		return "\"" + escaped + "\""
	}
	return escaped
}

func escapeQueryStringTerm(term string) string {
	const special = `+-=&|><!(){}[]^"~*?:\\/`
	var b strings.Builder
	b.Grow(len(term) * 2)
	for _, r := range term {
		if strings.ContainsRune(special, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// BuildFromIndexedFiles backfills the sidecar index from existing indexed-file
// metadata so full-text mode is usable on already-populated databases.
func BuildFromIndexedFiles(ctx context.Context, db *badger.DB) error {
	index, err := createOrOpen(db)
	if err != nil {
		return err
	}
	defer index.Close()

	ids := make([][]byte, 0, 1024)
	err = db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		prefix := []byte(cnst.IdxFileNamespace)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			ids = append(ids, it.Item().KeyCopy(nil))
		}
		return nil
	})
	if err != nil {
		return err
	}

	bar := progressbar.NewOptions64(
		int64(len(ids)),
		progressbar.OptionSetDescription("building full-text index (names + extracted content)"),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionSetTheme(cnst.CommonProgressBarTheme),
	)

	for _, fid := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		ifile, err := dbio.GetIndexedFile(fid, db)
		if err != nil {
			bar.Add(1) //nolint:errcheck
			continue
		}
		fileID := string(fid)
		if err := indexNamesWithIndex(index, fileID, ifile.Name, "indexed"); err != nil {
			bar.Add(1) //nolint:errcheck
			continue
		}
		text := extractTextForFTS(db, fid, defaultMaxIndexBytes, defaultMaxExtractedRunes)
		if text == "" {
			bar.Add(1) //nolint:errcheck
			continue
		}
		_ = index.Index(fileID+"#text", map[string]string{
			"content":  text,
			"doc_type": "indexed_text",
		})
		bar.Add(1) //nolint:errcheck
	}
	bar.Finish()
	fmt.Fprintln(os.Stderr)
	return nil
}

func indexNamesWithIndex(index bleve.Index, fileID string, name string, docType string) error {
	if name == "" {
		return nil
	}

	var b strings.Builder
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(strings.ReplaceAll(name, "/", " "))
	b.WriteByte(' ')
	b.WriteString(strings.ReplaceAll(name, "_", " "))
	b.WriteByte(' ')

	doc := map[string]string{
		"content":  b.String(),
		"doc_type": docType,
	}
	return index.Index(fileID, doc)
}

func TopCandidateIDs(scores map[string]float64) map[string]struct{} {
	if len(scores) == 0 {
		return nil
	}
	ids := make(map[string]struct{}, len(scores))
	for id := range scores {
		baseID := id
		baseID = strings.TrimSuffix(baseID, "#text")
		ids[baseID] = struct{}{}
	}
	return ids
}

func extractTextForFTS(db *badger.DB, fid []byte, maxBytes int64, maxRunes int) string {
	if maxBytes <= 0 || maxRunes <= 0 {
		return ""
	}
	remaining := maxBytes
	var out strings.Builder
	out.Grow(8 * 1024)

	_ = dbio.StreamLogicalFileBytes(fid, db, func(chunk []byte) error {
		if remaining <= 0 || out.Len() >= maxRunes {
			return context.Canceled
		}
		if int64(len(chunk)) > remaining {
			chunk = chunk[:remaining]
		}
		remaining -= int64(len(chunk))

		if !utf8.Valid(chunk) || !isLikelyText(chunk) {
			return nil
		}
		for len(chunk) > 0 {
			r, size := utf8.DecodeRune(chunk)
			if r == utf8.RuneError && size == 1 {
				chunk = chunk[size:]
				continue
			}
			if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsSpace(r) || strings.ContainsRune("._-/:\\", r) {
				out.WriteRune(unicode.ToLower(r))
			} else {
				out.WriteByte(' ')
			}
			if out.Len() >= maxRunes {
				return context.Canceled
			}
			chunk = chunk[size:]
		}
		out.WriteByte(' ')
		return nil
	})

	return strings.TrimSpace(out.String())
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
