package near

import (
	"context"
	"encoding/base64"
	"sort"

	"indicer/lib/cnst"
	"indicer/lib/dbio"

	"github.com/dgraph-io/badger/v4"
)

// verifyConfig holds options for Phase 2 full-file SimHash re-ranking.
type verifyConfig struct {
	enabled   bool
	topK      int
	querySig  uint64
	isOutfile bool
}

// computeAlignmentDeviation estimates the fraction of Phase 1 chunk-level simhash
// comparisons that are affected by partial edge-chunk alignment for the given file.
//
// With fixed-offset chunking, a logical file can cover at most 2 "edge" chunks where
// its byte range only partially occupies its chunk (stride bytes, the chunk size of
// its evidence). The simhash of that chunk includes bytes outside the logical file,
// injecting noise into the similarity score.
//
// Returns:
//   - edgeChunks: 0, 1 or 2 — how many partial-coverage boundary chunks exist.
//   - deviation: edgeChunks / totalChunks  (0.0 = perfectly aligned, 1.0 = worst case).
//   - warning: human-readable explanation, empty when deviation is zero.
func computeAlignmentDeviation(start, size, stride int64) (edgeChunks int, deviation float64, warning string) {
	if size <= 0 || stride <= 0 {
		return
	}
	totalChunks := (size + stride - 1) / stride
	startPartial := start%stride != 0
	endPartial := (start+size)%stride != 0

	if totalChunks == 1 {
		// A single chunk: it is an edge chunk only if it does not perfectly fill the block.
		// A file that starts on a chunk boundary and ends exactly at the next boundary
		// has zero partial coverage and zero alignment noise.
		if startPartial || endPartial {
			edgeChunks = 1
		}
	} else {
		if startPartial {
			edgeChunks++
		}
		if endPartial {
			edgeChunks++
		}
	}

	deviation = float64(edgeChunks) / float64(totalChunks)

	switch {
	case deviation == 0:
		// All chunks are fully aligned — no noise, no warning needed.
	case deviation < 0.1:
		warning = "Low alignment noise. Chunk-level similarity scores are reliable."
	case deviation < 0.3:
		warning = "Moderate alignment noise. Consider --advanced-deep for improved re-ranking."
	default:
		warning = "High alignment noise (small or misaligned file). Phase 1 scores may be inaccurate. --advanced-deep strongly recommended."
	}
	return
}

// computeDynamicTopKWithReason selects K for Phase 2 verification from the memory
// and threads available, as proxies for the I/O load the host tolerates, and returns
// the freeGB and threadCount values that drove the selection, so callers can print
// a human-readable explanation of the auto-selection basis.
func computeDynamicTopKWithReason(matchCount int) (k int, freeGB float64, threads int) {
	const minK = 5
	const maxK = 100

	cacheBytes, err := cnst.GetCacheLimit()
	freeGB = 2.0
	if err == nil {
		freeGB = float64(cacheBytes) / float64(cnst.GB)
	}
	threads = cnst.GetMaxThreadCount()

	k = int(freeGB * float64(threads))
	if k < minK {
		k = minK
	}
	if k > maxK {
		k = maxK
	}
	if k > matchCount {
		k = matchCount
	}
	return
}

// verifyPhase2 computes full-file SimHash similarity between querySig and each candidate
// in topKMatches by streaming their stored chunk bytes from the database. The slice is
// re-ranked in-place by descending Phase 2 score and each entry's Phase2Rank is assigned.
//
// Streaming is O(1) memory per candidate (accumulator holds only weights[64] + a 3-byte
// carry buffer), so K can safely scale to tens or hundreds of candidates.
//
// A candidate that cannot be read keeps a zero Phase 2 score, so it sorts to the
// bottom of the Phase 2 ranking; it does not abort the verification pass.
func verifyPhase2(ctx context.Context, querySig uint64, topKMatches []nearReportMatch, db *badger.DB) error {
	for i := range topKMatches {
		rawID, err := base64.StdEncoding.DecodeString(topKMatches[i].ID)
		if err != nil {
			return err
		}

		var candidateSig uint64

		// Cache-first: avoid re-streaming chunk bytes when the file simhash is
		// already stored from a previous --advanced-deep run.
		if cached, cacheErr := dbio.GetFileSimhash(rawID, db); cacheErr == nil {
			candidateSig = cached
		} else {
			sig, ok, err := storedSimhash(ctx, db, rawID)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				continue
			}
			if !ok {
				continue
			}
			candidateSig = sig
		}

		sim := hammingSimilarity64(querySig, candidateSig)
		topKMatches[i].Phase2FileSimilarity = sim
		topKMatches[i].Phase2FileSimilarityPct = sim * 100
	}

	// Re-rank the top-K subset by Phase 2 similarity (descending).
	// SliceStable preserves the Phase 1 order for ties.
	sort.SliceStable(topKMatches, func(i, j int) bool {
		return topKMatches[i].Phase2FileSimilarity > topKMatches[j].Phase2FileSimilarity
	})
	for i := range topKMatches {
		topKMatches[i].Phase2Rank = i + 1
	}
	return nil
}
