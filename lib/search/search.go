package search

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"indicer/internal/progress"
	"io/fs"
	"os"
	"strings"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/fsx"
	"indicer/lib/fts"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// DefaultReportPath is where a search writes its report when no path is
// given.
const DefaultReportPath = "report.json"

// ftsCandidateLimit is the most indexed files the full-text index offers to
// one search.
const ftsCandidateLimit = 5000

// ErrReportExists is a report path that already exists, without Force.
var ErrReportExists = errors.New("report file already exists")

// SearchSummaryWithContext counts query, taken as one literal term, in every
// stored object of db. It returns each object's count, keyed by base64 of
// its record key, and the occurrences in the evidence: the stored bytes
// counted once, not once per level (SRC-9).
func SearchSummaryWithContext(ctx context.Context, query string, db *badger.DB) (map[string]int64, int64, error) {
	res, err := Run(ctx, db, []string{query}, Options{})
	if err != nil {
		return nil, 0, err
	}
	out := make(map[string]int64, len(res.Hits))
	for _, h := range res.Hits {
		out[base64.StdEncoding.EncodeToString(h.Key)] = h.Counts[0]
	}
	return out, res.LevelTotal(LevelEvidence, 0), nil
}

// ValidateQuery checks query as a search would, without reading anything.
func ValidateQuery(query string) error { return validateTerms(parseQuery(query).terms) }

// Request is a search whose ranked results go to a report file.
type Request struct {
	// Query is split into terms: words are ANDed, "a | b" ORs them, and a
	// double-quoted phrase is one term.
	Query string
	// FullText searches only the indexed files the full-text index offers
	// as candidates, and falls back to a full search when it offers none.
	FullText bool
	// RankAlpha weighs raw occurrence counts against BM25 in the ranking.
	RankAlpha float64
	// Out is the report path (DefaultReportPath when empty); an existing
	// file is replaced only with Force.
	Out   string
	Force bool
}

// Outcome is what a search found and where its report went.
type Outcome struct {
	ReportPath string
	// Mode is "full-text" when the full-text candidates were searched, or
	// "scan" when every stored object was.
	Mode   string
	Result *Result
	Ranked int
	// Notes are what the user should know about how the search ran.
	Notes []string
}

// Modes of a search.
const (
	ModeScan     = "scan"
	ModeFullText = "full-text"
)

// SearchReport runs req and writes its report. Nothing is searched when the
// report path exists and req.Force is not set (ErrReportExists).
func SearchReport(ctx context.Context, db *badger.DB, req Request) (Outcome, error) {
	out := Outcome{ReportPath: req.Out, Mode: ModeScan}
	if out.ReportPath == "" {
		out.ReportPath = DefaultReportPath
	}
	pq := parseQuery(req.Query)
	if err := validateTerms(pq.terms); err != nil {
		return out, err
	}
	if err := checkReportPath(out.ReportPath, req.Force); err != nil {
		return out, err
	}

	var scores map[string]float64
	opts := Options{}
	if req.FullText {
		var notes []string
		var cands map[string]struct{}
		cands, scores, notes = fullTextCandidates(ctx, db, pq)
		out.Notes = append(out.Notes, notes...)
		if cands != nil {
			opts.Candidates, out.Mode = cands, ModeFullText
		}
	}
	name := "search"
	if out.Mode == ModeFullText {
		name = "full-text search"
	}
	ph := progress.Start(ctx, name, progress.Objects, progress.Unknown)
	opts.Progress = ph
	res, err := Run(ctx, db, pq.terms, opts)
	if err == nil && out.Mode == ModeFullText && len(res.Hits) == 0 {
		out.Notes = append(out.Notes, "the full-text candidates hold no match; searched every stored object instead")
		out.Mode, opts.Candidates, scores = ModeScan, nil, nil
		ph.Done(nil)
		ph = progress.Start(ctx, "search", progress.Objects, progress.Unknown)
		opts.Progress = ph
		res, err = Run(ctx, db, pq.terms, opts)
	}
	ph.Done(err)
	if err != nil {
		return out, err
	}
	out.Result = res

	ranked := rankHits(res, pq.op, req.RankAlpha)
	if len(scores) > 0 {
		ranked = applyFTSBoost(ranked, scores, req.RankAlpha)
	}
	out.Ranked = len(ranked)
	report, err := buildReport(db, req.Query, pq, out.Mode, res, ranked)
	if err != nil {
		return out, err
	}
	body, err := json.MarshalIndent(report, "", "\t")
	if err != nil {
		return out, err
	}
	err = fsx.WriteAtomic(out.ReportPath, cnst.FilePerm, fsx.Options{NoOverwrite: !req.Force, RemoveStale: true}, func(f *os.File) error {
		_, err := f.Write(body)
		return err
	})
	if errors.Is(err, fs.ErrExist) {
		return out, fmt.Errorf("%w: %s was created while searching; pass --force to replace it", ErrReportExists, out.ReportPath)
	}
	return out, err
}

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

// fullTextCandidates asks the full-text index for the indexed files that may
// hold the query. It returns nil candidates, with a note saying why, when
// the search must read every object instead.
func fullTextCandidates(ctx context.Context, db *badger.DB, pq parsedQuery) (map[string]struct{}, map[string]float64, []string) {
	mode := fts.QueryModeAnd
	if pq.op == queryOpOr {
		mode = fts.QueryModeOr
	}
	c, err := fts.SearchCandidates(ctx, db, pq.terms, mode, ftsCandidateLimit)
	switch {
	case errors.Is(err, fts.ErrIndexNotReady):
		return nil, nil, []string{"this database has no full-text index (store or reindex with --fts to build one); searched every stored object instead"}
	case err != nil:
		return nil, nil, []string{fmt.Sprintf("the full-text index could not be searched (%v); searched every stored object instead", err)}
	case len(c.Scores) == 0:
		return nil, nil, []string{"the full-text index offers no candidate for this query; searched every stored object instead"}
	}
	cands := c.IDs()
	notes := []string{fmt.Sprintf("full-text mode: searched only the %d indexed files the full-text index offers; evidence and partitions, and files whose names and first %d KiB of text do not hold the query's words, were not searched", len(cands), fts.MaxTextBytes/1024)}
	if c.Capped() {
		notes = append(notes, fmt.Sprintf("the full-text index matched %d documents and offers at most %d: more files may hold the query", c.Total, ftsCandidateLimit))
	}
	return cands, c.FileScores(), notes
}

// rankHits ranks the hits with BM25 over the objects the search read.
func rankHits(res *Result, op queryOp, alpha float64) []RankedResult {
	hitMaps := make([]map[string]int, len(res.Terms))
	sizes := make(map[string]int64, len(res.Hits))
	for i := range hitMaps {
		hitMaps[i] = map[string]int{}
	}
	for _, h := range res.Hits {
		for i, n := range h.Counts {
			if n > 0 {
				hitMaps[i][string(h.Key)] = int(n)
			}
		}
		sizes[string(h.Key)] = h.Size
	}
	c := res.Coverage
	return rankBM25(hitMaps, sizes, c.Evidence+c.Partitions+c.Files, op, alpha)
}

// applyFTSBoost adds a share of each file's full-text score to its BM25 score
// and sorts again as rankBM25 does, so --rank-alpha still applies.
func applyFTSBoost(ranked []RankedResult, ftScores map[string]float64, alpha float64) []RankedResult {
	if len(ranked) == 0 || len(ftScores) == 0 {
		return ranked
	}
	for index := range ranked {
		if score, ok := ftScores[ranked[index].FileID]; ok {
			ranked[index].Score += 0.05 * score
		}
	}
	sortRankedResults(ranked, alpha)
	return ranked
}

// reportBuilder names the objects of a report from their records, through
// the record helpers rather than by splitting names on the separator, which
// a path may contain (SRC-8).
type reportBuilder struct {
	db       *badger.DB
	evidence map[string][]string // raw evidence ID -> its name and its aliases' paths
	parts    map[string]string   // raw partition ID -> its name
}

func buildReport(db *badger.DB, query string, pq parsedQuery, mode string, res *Result, ranked []RankedResult) (structs.SearchReport, error) {
	b := &reportBuilder{db: db, evidence: map[string][]string{}, parts: map[string]string{}}
	report := structs.SearchReport{
		SchemaVersion: structs.SearchReportSchemaVersion,
		Query:         query,
		Terms:         pq.terms,
		Operator:      "and",
		Mode:          mode,
		Occurrences:   []structs.OccurrenceData{},
	}
	if pq.op == queryOpOr {
		report.Operator = "or"
	}
	hits := make(map[string]Hit, len(res.Hits))
	for _, h := range res.Hits {
		hits[string(h.Key)] = h
	}
	for _, rr := range ranked {
		h := hits[rr.FileID]
		occ, err := b.occurrence(h, rr, res.Terms)
		if err != nil {
			return report, err
		}
		report.Occurrences = append(report.Occurrences, occ)
	}

	levels := map[Level]*structs.LevelSummary{
		LevelEvidence:  &report.Summary.Evidence,
		LevelPartition: &report.Summary.Partitions,
		LevelIndexed:   &report.Summary.IndexedFiles,
	}
	for _, h := range res.Hits {
		s := levels[h.Level]
		s.Objects++
		for _, n := range h.Counts {
			s.Occurrences += n
		}
	}
	c := res.Coverage
	report.Coverage = structs.SearchCoverage{
		EvidenceSearched: c.Evidence, PartitionsSearched: c.Partitions, FilesSearched: c.Files, BytesSearched: c.Bytes,
		NotStored: c.NotStored, Quarantined: c.Quarantined, NotCandidates: c.NotCandidates,
		Unavailable: c.Unavailable, Unreadable: c.Unreadable, Problems: c.Problems,
	}
	report.ExecutiveSummary = executiveSummary(query, mode, report)
	return report, nil
}

func executiveSummary(query, mode string, r structs.SearchReport) string {
	s := r.Summary
	var text string
	if mode == ModeFullText {
		text = fmt.Sprintf("%d occurrences of '%s' were found in %d indexed files, searching only the files the full-text index offered.",
			s.IndexedFiles.Occurrences, query, s.IndexedFiles.Objects)
	} else {
		text = fmt.Sprintf("%d occurrences of '%s' were found in %d of the %d evidence searched (%d bytes); they lie in %d partitions and %d indexed files.",
			s.Evidence.Occurrences, query, s.Evidence.Objects, r.Coverage.EvidenceSearched, r.Coverage.BytesSearched, s.Partitions.Objects, s.IndexedFiles.Objects)
	}
	if n := r.Coverage.Unreadable; n > 0 {
		text += fmt.Sprintf(" %d objects could not be read and were not searched.", n)
	}
	return text
}

func (b *reportBuilder) occurrence(h Hit, rr RankedResult, terms []string) (structs.OccurrenceData, error) {
	raw := rawID(h.Key)
	occ := structs.OccurrenceData{
		ArtefactHash: base64.StdEncoding.EncodeToString(raw),
		Level:        string(h.Level),
		Size:         h.Size,
		Count:        rr.TF,
		BM25Score:    rr.Score,
	}
	if len(terms) > 1 {
		occ.TermCounts = make(map[string]int64, len(terms))
		for i, t := range terms {
			occ.TermCounts[t] = h.Counts[i]
		}
	}
	evNames, err := b.evidenceNames(h.EvidenceID)
	if err != nil {
		return occ, err
	}
	ev := &structs.EvidenceFilePart{EvidenceFileHash: base64.StdEncoding.EncodeToString(h.EvidenceID), EvidenceFileNames: evNames}
	switch h.Level {
	case LevelEvidence:
		occ.FileNames = evNames[:1]
	case LevelPartition:
		name, err := b.partitionName(raw)
		if err != nil {
			return occ, err
		}
		occ.FileNames = []string{name}
		ev.Partition = &structs.PartitionPart{PartitionHash: occ.ArtefactHash, PartitionPartNames: []string{name}}
	case LevelIndexed:
		rec, err := dbio.GetIndexedFile(h.Key, b.db)
		if err != nil {
			return occ, fmt.Errorf("indexed record %q: %w", h.Key, err)
		}
		path := dbio.RecordPath(rec)
		occ.Path, occ.FileNames = path, []string{path}
		pid, err := dbio.RecordPartitionID(rec)
		if err != nil {
			return occ, err
		}
		pname, err := b.partitionName(pid)
		if err != nil {
			return occ, err
		}
		ev.Partition = &structs.PartitionPart{
			PartitionHash:      base64.StdEncoding.EncodeToString(pid),
			PartitionPartNames: []string{pname},
			Indexed:            &structs.IndexedPart{IndexedFileHash: occ.ArtefactHash, IndexedFileNames: []string{path}},
		}
	}
	occ.EvidenceFile = ev
	return occ, nil
}

// evidenceNames is an evidence's name, then the paths it was stored again
// from (its aliases, D1).
func (b *reportBuilder) evidenceNames(eid []byte) ([]string, error) {
	if names, ok := b.evidence[string(eid)]; ok {
		return names, nil
	}
	e, err := dbio.GetEvidenceFile(eid, b.db)
	if err != nil {
		return nil, fmt.Errorf("evidence %x: %w", eid, err)
	}
	names := []string{e.Name}
	aliases, err := dbio.Aliases(b.db, eid)
	if err != nil {
		return nil, err
	}
	for _, a := range aliases {
		if a.Path != "" && !strings.EqualFold(a.Path, e.Name) {
			names = append(names, a.Path)
		}
	}
	b.evidence[string(eid)] = names
	return names, nil
}

func (b *reportBuilder) partitionName(pid []byte) (string, error) {
	if name, ok := b.parts[string(pid)]; ok {
		return name, nil
	}
	prec, err := dbio.GetPartitionFile(pid, b.db)
	if err != nil {
		return "", fmt.Errorf("partition %x: %w", pid, err)
	}
	name := dbio.RecordPath(prec.IndexedFile)
	b.parts[string(pid)] = name
	return name, nil
}

// rawID is a record key without its namespace.
func rawID(key []byte) []byte {
	for _, ns := range []string{cnst.EviFileNamespace, cnst.PartiFileNamespace, cnst.IdxFileNamespace} {
		if bytes.HasPrefix(key, []byte(ns)) {
			return key[len(ns):]
		}
	}
	return key
}
