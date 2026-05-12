package search

import (
	"math"
	"sort"
)

// BM25 tuning parameters (Okapi BM25).
const (
	bm25K1                      float64 = 1.5  // controls term-frequency saturation
	bm25B                       float64 = 0.75 // controls document-length normalization
	defaultOccurrenceBoostAlpha float64 = 0.35
)

var occurrenceBoostAlpha = defaultOccurrenceBoostAlpha

// SetOccurrenceBoostAlpha configures how strongly raw occurrence frequency
// nudges ranking when BM25 scores are close.
func SetOccurrenceBoostAlpha(alpha float64) {
	if alpha < 0 {
		alpha = 0
	}
	occurrenceBoostAlpha = alpha
}

func GetOccurrenceBoostAlpha() float64 {
	return occurrenceBoostAlpha
}

// RankedResult holds a file ID, its BM25 score, and the aggregate raw
// term count for a matched document.
type RankedResult struct {
	FileID string
	Score  float64
	TF     int
}

// rankBM25 scores and ranks documents using Okapi BM25.
//
// hitMaps contains one map per query term: fileID -> raw occurrence count.
// docSizes maps fileID -> document byte size for length normalization.
// totalDocs is the size of the indexed corpus used for IDF computation.
// op controls whether AND (intersection) or OR (union) candidate selection
// is used when combining results from multiple terms.
func rankBM25(hitMaps []map[string]int, docSizes map[string]int64, totalDocs int64, op queryOp) []RankedResult {
	if len(hitMaps) == 0 || totalDocs == 0 {
		return nil
	}

	candidates := buildCandidateSet(hitMaps, op)
	if len(candidates) == 0 {
		return nil
	}

	avgLen := computeAverageDocLen(candidates, docSizes)
	scores := make(map[string]float64, len(candidates))
	tfSum := make(map[string]int, len(candidates))

	for _, hits := range hitMaps {
		df := int64(len(hits))
		if df == 0 {
			continue
		}
		// Robertson-Spärck Jones IDF with +1 smoothing to keep IDF positive.
		idf := math.Log(1 + (float64(totalDocs)-float64(df)+0.5)/(float64(df)+0.5))

		for id := range candidates {
			tf, ok := hits[id]
			if !ok {
				continue
			}
			normLen := safeNormalizedLen(docSizes[id], avgLen)
			tfF := float64(tf)
			tfNorm := tfF * (bm25K1 + 1) / (tfF + bm25K1*(1-bm25B+bm25B*normLen))
			scores[id] += idf * tfNorm
			tfSum[id] += tf
		}
	}

	results := make([]RankedResult, 0, len(scores))
	for id, score := range scores {
		results = append(results, RankedResult{FileID: id, Score: score, TF: tfSum[id]})
	}
	sortRankedResults(results)
	return results
}

// sortRankedResults keeps ordering stable and occurrence-aware:
// 1) Higher BM25 score first
// 2) Higher occurrence count (TF) first
// 3) Lexicographically smaller file ID first (deterministic output)
func sortRankedResults(results []RankedResult) {
	sort.Slice(results, func(i, j int) bool {
		ri := rankScore(results[i])
		rj := rankScore(results[j])
		if ri != rj {
			return ri > rj
		}
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		if results[i].TF != results[j].TF {
			return results[i].TF > results[j].TF
		}
		return results[i].FileID < results[j].FileID
	})
}

func rankScore(result RankedResult) float64 {
	return result.Score + occurrenceBoostAlpha*math.Log1p(float64(result.TF))
}

// buildCandidateSet returns the set of file IDs to score.
// AND: intersection — only files that appear in every hit map.
// OR:  union — files that appear in any hit map.
func buildCandidateSet(hitMaps []map[string]int, op queryOp) map[string]struct{} {
	if len(hitMaps) == 0 {
		return nil
	}
	if op == queryOpOr {
		out := make(map[string]struct{})
		for _, hits := range hitMaps {
			for id := range hits {
				out[id] = struct{}{}
			}
		}
		return out
	}

	// AND: start from the smallest hit map and intersect outward.
	smallest := 0
	for i, hits := range hitMaps {
		if len(hits) < len(hitMaps[smallest]) {
			smallest = i
		}
	}
	out := make(map[string]struct{}, len(hitMaps[smallest]))
	for id := range hitMaps[smallest] {
		out[id] = struct{}{}
	}
	for i, hits := range hitMaps {
		if i == smallest {
			continue
		}
		for id := range out {
			if _, ok := hits[id]; !ok {
				delete(out, id)
			}
		}
	}
	return out
}

func computeAverageDocLen(candidates map[string]struct{}, docSizes map[string]int64) float64 {
	var total int64
	var count int64
	for id := range candidates {
		if sz, ok := docSizes[id]; ok && sz > 0 {
			total += sz
			count++
		}
	}
	if count == 0 {
		return 1
	}
	return float64(total) / float64(count)
}

func safeNormalizedLen(sizeBytes int64, avgLen float64) float64 {
	if sizeBytes <= 0 || avgLen <= 0 {
		return 1
	}
	return float64(sizeBytes) / avgLen
}
