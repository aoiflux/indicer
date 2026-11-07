package near

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
	"sort"
	"strings"
	"time"

	"indicer/internal/console"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/engine"
	"indicer/lib/fsx"

	"github.com/dgraph-io/badger/v4"
)

// now is the clock a search is timed by; tests replace it.
var now = time.Now

const shallowExactRankWeight = 1.35
const deepSimhashRankWeight = 1.00

type nearJSONReport struct {
	GeneratedAt       string            `json:"generated_at"`
	DurationMs        int64             `json:"duration_ms"`
	DeepMode          bool              `json:"deep_mode"`
	VerifyMode        bool              `json:"advanced_deep_mode"`
	VerifyTopK        int               `json:"verify_top_k,omitempty"`
	VerifyDurationMs  int64             `json:"verify_duration_ms,omitempty"`
	SimilarityWarning string            `json:"similarity_warning,omitempty"`
	Notes             []string          `json:"notes,omitempty"`
	Coverage          Coverage          `json:"coverage"`
	Input             nearReportInput   `json:"input"`
	Summary           nearReportSummary `json:"summary"`
	Matches           []nearReportMatch `json:"matches"`
}

type nearReportInput struct {
	QueryHashBase64 string   `json:"query_hash_base64"`
	QueryID         string   `json:"query_id"`
	QueryType       string   `json:"query_type"`
	QueryNames      []string `json:"query_names,omitempty"`
	ExactFileMatch  bool     `json:"exact_file_match,omitempty"`
	ExplainExact    bool     `json:"explain_exact,omitempty"`
}

type nearReportSummary struct {
	TotalMatches int `json:"total_matches"`
	Evidence     int `json:"evidence_matches"`
	Partition    int `json:"partition_matches"`
	Indexed      int `json:"indexed_matches"`
}

type nearReportMatch struct {
	Rank        int      `json:"rank"`
	ID          string   `json:"id"`
	Type        string   `json:"type"`
	ExactMatch  bool     `json:"exact_match"`
	MatchMethod string   `json:"match_method,omitempty"`
	HashBase64  string   `json:"hash_base64"`
	Names       []string `json:"names,omitempty"`
	Start       int64    `json:"start"`
	End         int64    `json:"end"`
	Size        int64    `json:"size"`
	// Confidence is the share of the object's chunks the query matched,
	// each weighted by how it matched, at most 1.
	Confidence        float64 `json:"confidence"`
	ConfidencePercent float64 `json:"confidence_percent"`
	// OverallRelatedness is the single synthesised score that represents how strongly
	// the two artefacts are related, taking into account every phase that was executed.
	// See RelatednessBasis for how the value was derived.
	OverallRelatedness    float64 `json:"overall_relatedness"`
	OverallRelatednessPct float64 `json:"overall_relatedness_percent"`
	// RelatednessBasis documents which signals were blended: "exact", "phase1", "phase1+phase2".
	RelatednessBasis   string `json:"relatedness_basis"`
	InternalObjectSize int    `json:"internal_object_size"`
	// ObjectChunks is how many chunks the object's bytes lie in;
	// ChunkMatchCount how many of them the query matched.
	ObjectChunks               int64            `json:"object_chunks,omitempty"`
	ChunkMatchCount            int              `json:"chunk_match_count"`
	DeepMatchCount             int              `json:"deep_match_count"`
	ShallowMatchCount          int              `json:"shallow_match_count"`
	ChunkConfidenceSum         float64          `json:"chunk_confidence_sum"`
	ChunkConfidenceAvg         float64          `json:"chunk_confidence_avg"`
	WeightedChunkConfidenceSum float64          `json:"weighted_chunk_confidence_sum"`
	WeightedChunkConfidenceAvg float64          `json:"weighted_chunk_confidence_avg"`
	MatchingChunks             []nearChunkMatch `json:"matching_chunks,omitempty"`
	// Phase 1 alignment noise estimate (0.0 = fully aligned, 1.0 = all chunks are edge chunks).
	Phase1DeviationEstimate float64 `json:"phase1_deviation_estimate"`
	Phase1DeviationWarning  string  `json:"phase1_deviation_warning,omitempty"`
	// Phase 2 full-file SimHash re-ranking fields (populated when --advanced-deep is used).
	Phase2FileSimilarity    float64 `json:"phase2_file_similarity,omitempty"`
	Phase2FileSimilarityPct float64 `json:"phase2_file_similarity_pct,omitempty"`
	Phase2Rank              int     `json:"phase2_rank,omitempty"`
}

// nearChunkMatch is one matched chunk of the query: where it is in the query.
type nearChunkMatch struct {
	Index             int64   `json:"index"`
	Method            string  `json:"method"`
	Confidence        float64 `json:"confidence"`
	ConfidencePercent float64 `json:"confidence_percent"`
}

// finish builds the report of a matched run, runs phase 2, and writes it.
func (r *run) finish(ctx context.Context, out Outcome, input nearReportInput, vcfg verifyConfig) (Outcome, error) {
	report := nearJSONReport{
		GeneratedAt: now().Format(time.RFC3339),
		DeepMode:    r.opts.Deep,
		VerifyMode:  vcfg.enabled,
		Input:       input,
		Matches:     []nearReportMatch{},
	}
	if vcfg.isOutfile && r.opts.Deep {
		report.SimilarityWarning = "Phase 1 deep-simhash scores compare fixed-size chunks. " +
			"The query file's chunk grid is unlikely to align with stored files' chunk grids, so confidence values are approximate. " +
			"Use --advanced-deep for full-file SimHash re-ranking, which is alignment-independent."
	} else if r.opts.Deep {
		report.SimilarityWarning = "Deep-simhash confidence values may be affected by partial edge-chunk alignment " +
			"(see phase1_deviation_estimate per match). Use --advanced-deep to re-rank by full-file SimHash."
	}

	keys := make([]string, 0, len(r.matches)+len(r.exact))
	for k := range r.exact {
		keys = append(keys, k)
	}
	for k := range r.matches {
		if _, exact := r.exact[k]; !exact {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		m, err := r.reportMatch([]byte(k))
		if err != nil {
			return out, err
		}
		report.Matches = append(report.Matches, m)
	}
	sortPhase1(report.Matches)
	for i := range report.Matches {
		report.Matches[i].Rank = i + 1
		report.Summary.TotalMatches++
		switch report.Matches[i].Type {
		case "evidence":
			report.Summary.Evidence++
		case "partition":
			report.Summary.Partition++
		case "indexed":
			report.Summary.Indexed++
		}
	}

	if vcfg.enabled && len(report.Matches) > 0 {
		verifyStart := now()
		k := vcfg.topK
		if k <= 0 {
			var freeGB float64
			var threads int
			k, freeGB, threads = computeDynamicTopKWithReason(engine.For(r.db).Tuning(), len(report.Matches))
			console.Printf("[advanced-deep] auto-selected top-K = %d "+
				"(%.1f GB available memory × %d CPU threads, bounded [5, 100], capped at %d total matches)\n",
				k, freeGB, threads, len(report.Matches))
		}
		k = min(k, len(report.Matches))
		report.VerifyTopK = k
		ph := progress.Start(ctx, "verify", progress.Unit("matches"), int64(k))
		err := verifyPhase2(progress.Within(ctx, ph), vcfg.querySig, report.Matches[:k], r.db)
		ph.Done(err)
		if err != nil {
			return out, err
		}
		report.VerifyDurationMs = now().Sub(verifyStart).Milliseconds()
	}
	for i := range report.Matches {
		computeOverallRelatedness(&report.Matches[i])
	}
	sort.SliceStable(report.Matches, func(i, j int) bool {
		return report.Matches[i].OverallRelatedness > report.Matches[j].OverallRelatedness
	})
	for i := range report.Matches {
		report.Matches[i].Rank = i + 1
	}

	out.Coverage = r.cov
	out.Notes = append(out.Notes, r.notes()...)
	report.Coverage, report.Notes = out.Coverage, out.Notes
	report.DurationMs = now().Sub(r.started).Milliseconds()
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return out, err
	}
	err = fsx.WriteAtomic(out.ReportPath, cnst.FilePerm, fsx.Options{NoOverwrite: !r.opts.Force, RemoveStale: true}, func(f *os.File) error {
		_, err := f.Write(body)
		return err
	})
	if errors.Is(err, fs.ErrExist) {
		return out, fmt.Errorf("%w: %s was created while searching; pass --force to replace it", ErrReportExists, out.ReportPath)
	}
	out.Matches = len(report.Matches)
	return out, err
}

func sortPhase1(ms []nearReportMatch) {
	sort.Slice(ms, func(i, j int) bool {
		a, b := ms[i], ms[j]
		switch {
		case a.ExactMatch != b.ExactMatch:
			return a.ExactMatch
		case a.WeightedChunkConfidenceSum != b.WeightedChunkConfidenceSum:
			return a.WeightedChunkConfidenceSum > b.WeightedChunkConfidenceSum
		case a.ChunkMatchCount != b.ChunkMatchCount:
			return a.ChunkMatchCount > b.ChunkMatchCount
		case a.Confidence != b.Confidence:
			return a.Confidence > b.Confidence
		case a.Size != b.Size:
			return a.Size > b.Size
		}
		return a.ID < b.ID
	})
}

// reportMatch describes one matched object, named from its record (SRC-8).
func (r *run) reportMatch(key []byte) (nearReportMatch, error) {
	m := nearReportMatch{
		ID:         base64.StdEncoding.EncodeToString(key),
		Type:       nearFileType(key),
		HashBase64: base64.StdEncoding.EncodeToString(rawID(key)),
		Names:      recordNames(r.db, key),
	}
	stride := int64(0)
	switch {
	case bytes.HasPrefix(key, []byte(cnst.IdxFileNamespace)):
		rec, err := dbio.GetIndexedFile(key, r.db)
		if err != nil {
			return m, err
		}
		m.Start, m.Size = rec.Start, rec.Size
	case bytes.HasPrefix(key, []byte(cnst.PartiFileNamespace)):
		rec, err := dbio.GetPartitionFile(key, r.db)
		if err != nil {
			return m, err
		}
		m.Start, m.Size, m.InternalObjectSize = rec.Start, rec.Size, dbio.PartitionMemberCount(rec)
	case bytes.HasPrefix(key, []byte(cnst.EviFileNamespace)):
		rec, err := dbio.GetEvidenceFile(key, r.db)
		if err != nil {
			return m, err
		}
		m.Start, m.Size, m.InternalObjectSize = rec.Start, rec.Size, len(rec.InternalObjects)
	}
	m.End = m.Start + m.Size

	if _, exact := r.exact[string(key)]; exact {
		m.ExactMatch, m.MatchMethod = true, methodExactFile
		m.Confidence, m.ConfidencePercent = 1, 100
	}
	if mt := r.matches[string(key)]; mt != nil {
		positions := make([]int64, 0, len(mt.best))
		for pos := range mt.best {
			positions = append(positions, pos)
		}
		sort.Slice(positions, func(i, j int) bool { return positions[i] < positions[j] })
		for _, pos := range positions {
			c, method := mt.best[pos], mt.method[pos]
			m.ChunkConfidenceSum += c
			m.WeightedChunkConfidenceSum += c * chunkMethodWeight(method)
			if method == methodDeep {
				m.DeepMatchCount++
			} else {
				m.ShallowMatchCount++
			}
		}
		m.ChunkMatchCount = len(positions)
		if m.ChunkMatchCount > 0 {
			m.ChunkConfidenceAvg = m.ChunkConfidenceSum / float64(m.ChunkMatchCount)
			m.WeightedChunkConfidenceAvg = m.WeightedChunkConfidenceSum / float64(m.ChunkMatchCount)
		}
		listed := append([]nearChunkMatch(nil), mt.listed...)
		sort.Slice(listed, func(i, j int) bool {
			if listed[i].Index != listed[j].Index {
				return listed[i].Index < listed[j].Index
			}
			return listed[i].Method < listed[j].Method
		})
		m.MatchingChunks = listed
		m.ObjectChunks = mt.target.chunks()
		if !m.ExactMatch {
			if m.MatchMethod == "" && len(listed) > 0 {
				m.MatchMethod = listed[0].Method
			}
			// A share of the object's chunks: never more than all of them
			// (SRC-9).
			m.Confidence = min(1, m.ChunkConfidenceSum/float64(max(m.ObjectChunks, 1)))
			m.ConfidencePercent = m.Confidence * 100
		}
		if mt.target.ev != nil {
			stride = mt.target.ev.stride
		} else if mt.target.obj != nil {
			stride = mt.target.obj.ChunkSize
		}
	}
	if !m.ExactMatch && stride > 0 {
		_, m.Phase1DeviationEstimate, m.Phase1DeviationWarning = computeAlignmentDeviation(m.Start, m.Size, stride)
	}
	return m, nil
}

// computeOverallRelatedness synthesises a single relatedness score from all
// phases that ran for this match and writes it into the match in-place.
//
// Formula:
//   - exact-file-hash  : 1.0  (identity — no blend needed)
//   - Phase 2 ran (Phase2Rank > 0): 0.60 × phase2_file_similarity + 0.40 × confidence
//     Phase 2 is alignment-independent and therefore the dominant signal; Phase 1
//     confidence captures chunk-level coverage and provides the remaining weight.
//   - Phase 1 only     : confidence  (best available signal)
//
// RelatednessBasis records which path was taken so consumers can interpret the value.
func computeOverallRelatedness(m *nearReportMatch) {
	switch {
	case m.MatchMethod == methodExactFile:
		m.OverallRelatedness = 1.0
		m.OverallRelatednessPct = 100.0
		m.RelatednessBasis = "exact"
	case m.Phase2Rank > 0:
		m.OverallRelatedness = 0.60*m.Phase2FileSimilarity + 0.40*m.Confidence
		m.OverallRelatednessPct = m.OverallRelatedness * 100
		m.RelatednessBasis = "phase1+phase2"
	default:
		m.OverallRelatedness = m.Confidence
		m.OverallRelatednessPct = m.Confidence * 100
		m.RelatednessBasis = "phase1"
	}
}

func chunkMethodWeight(method string) float64 {
	switch method {
	case methodExact:
		return shallowExactRankWeight
	case methodDeep:
		return deepSimhashRankWeight
	}
	return 1
}

func nearFileType(id []byte) string {
	switch {
	case bytes.HasPrefix(id, []byte(cnst.EviFileNamespace)):
		return "evidence"
	case bytes.HasPrefix(id, []byte(cnst.PartiFileNamespace)):
		return "partition"
	case bytes.HasPrefix(id, []byte(cnst.IdxFileNamespace)):
		return "indexed"
	}
	return "unknown"
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

// recordNames is the name of the object key, from its record: an indexed
// file's path or a partition's name through the record helpers, never by
// splitting a name on the separator a path may hold (SRC-8).
func recordNames(db *badger.DB, key []byte) []string {
	var name string
	switch {
	case bytes.HasPrefix(key, []byte(cnst.IdxFileNamespace)):
		if rec, err := dbio.GetIndexedFile(key, db); err == nil {
			name = dbio.RecordPath(rec)
		}
	case bytes.HasPrefix(key, []byte(cnst.PartiFileNamespace)):
		if rec, err := dbio.GetPartitionFile(key, db); err == nil {
			name = dbio.RecordPath(rec.IndexedFile)
		}
	case bytes.HasPrefix(key, []byte(cnst.EviFileNamespace)):
		if rec, err := dbio.GetEvidenceFile(key, db); err == nil {
			name = rec.Name
		}
	}
	if strings.TrimSpace(name) == "" {
		return nil
	}
	return []string{name}
}
