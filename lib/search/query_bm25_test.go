package search

import (
	"errors"
	"strings"
	"testing"
)

// --- query parser tests ---

func TestParseQuerySingleTerm(t *testing.T) {
	t.Parallel()
	pq := parseQuery("foo")
	if len(pq.terms) != 1 || pq.terms[0] != "foo" {
		t.Fatalf("expected [foo], got %v", pq.terms)
	}
	if pq.op != queryOpAnd {
		t.Fatal("expected AND operator for single term")
	}
}

func TestParseQueryAndMultiWord(t *testing.T) {
	t.Parallel()
	pq := parseQuery("foo bar")
	if len(pq.terms) != 2 || pq.terms[0] != "foo" || pq.terms[1] != "bar" {
		t.Fatalf("expected [foo bar], got %v", pq.terms)
	}
	if pq.op != queryOpAnd {
		t.Fatal("expected AND operator for multi-word")
	}
}

func TestParseQueryOrOperator(t *testing.T) {
	t.Parallel()
	pq := parseQuery("foo|bar")
	if len(pq.terms) != 2 || pq.terms[0] != "foo" || pq.terms[1] != "bar" {
		t.Fatalf("expected [foo bar], got %v", pq.terms)
	}
	if pq.op != queryOpOr {
		t.Fatal("expected OR operator for pipe-separated terms")
	}
}

func TestParseQueryPhraseTerm(t *testing.T) {
	t.Parallel()
	pq := parseQuery(`"foo bar"`)
	if len(pq.terms) != 1 || pq.terms[0] != "foo bar" {
		t.Fatalf("expected single phrase term [foo bar], got %v", pq.terms)
	}
	if pq.op != queryOpAnd {
		t.Fatal("expected AND operator for phrase")
	}
}

func TestParseQueryNormalizesCase(t *testing.T) {
	t.Parallel()
	pq := parseQuery("FOO BAR")
	for _, term := range pq.terms {
		for _, r := range term {
			if r >= 'A' && r <= 'Z' {
				t.Fatalf("expected lowercase terms, got %v", pq.terms)
			}
		}
	}
}

func TestValidateTermsRejectsShortTerm(t *testing.T) {
	t.Parallel()
	if err := validateTerms([]string{"a"}); err == nil {
		t.Fatal("expected error for single-char term, got nil")
	}
}

func TestValidateTermsRejectsEmpty(t *testing.T) {
	t.Parallel()
	if err := validateTerms(nil); err == nil {
		t.Fatal("expected error for empty terms, got nil")
	}
}

// TestValidateTermsCapsQueries (SRC-2): a term longer than MaxTermBytes, or
// more than MaxTerms terms, is refused before anything is read.
func TestValidateTermsCapsQueries(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", MaxTermBytes+1)
	if err := validateTerms([]string{long}); !errors.Is(err, ErrQueryTooLong) {
		t.Fatalf("a %d-byte term: %v, want ErrQueryTooLong", len(long), err)
	}
	if err := validateTerms([]string{strings.Repeat("x", MaxTermBytes)}); err != nil {
		t.Fatalf("a %d-byte term: %v", MaxTermBytes, err)
	}
	many := make([]string, MaxTerms+1)
	for i := range many {
		many[i] = "ab"
	}
	if err := validateTerms(many); !errors.Is(err, ErrQueryTooLong) {
		t.Fatalf("%d terms: %v, want ErrQueryTooLong", len(many), err)
	}
}

// --- BM25 tests ---

func TestRankBM25ReturnsNilForEmptyHits(t *testing.T) {
	t.Parallel()
	result := rankBM25([]map[string]int{{}}, map[string]int64{}, 10, queryOpAnd, DefaultRankAlpha)
	if result != nil {
		t.Fatalf("expected nil for empty hits, got %v", result)
	}
}

func TestRankBM25ReturnsNilForZeroTotalDocs(t *testing.T) {
	t.Parallel()
	hits := map[string]int{"a": 5}
	result := rankBM25([]map[string]int{hits}, map[string]int64{"a": 100}, 0, queryOpAnd, DefaultRankAlpha)
	if result != nil {
		t.Fatalf("expected nil for zero totalDocs, got %v", result)
	}
}

func TestRankBM25ScoresDescending(t *testing.T) {
	t.Parallel()
	// doc "a" has more occurrences than doc "b" with same size, expect higher score
	hits := map[string]int{"a": 10, "b": 1}
	sizes := map[string]int64{"a": 1000, "b": 1000}
	results := rankBM25([]map[string]int{hits}, sizes, 100, queryOpAnd, DefaultRankAlpha)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].FileID != "a" {
		t.Fatalf("expected 'a' first (highest score), got %q", results[0].FileID)
	}
	if results[0].Score <= results[1].Score {
		t.Fatalf("expected descending scores, got %f <= %f", results[0].Score, results[1].Score)
	}
}

func TestRankBM25ANDIntersects(t *testing.T) {
	t.Parallel()
	term1 := map[string]int{"a": 5, "b": 3}
	term2 := map[string]int{"b": 2, "c": 1}
	sizes := map[string]int64{"a": 1000, "b": 1000, "c": 1000}
	results := rankBM25([]map[string]int{term1, term2}, sizes, 100, queryOpAnd, DefaultRankAlpha)

	// AND: only "b" appears in both
	if len(results) != 1 || results[0].FileID != "b" {
		t.Fatalf("expected only 'b' in AND results, got %v", results)
	}
}

func TestRankBM25ORUnion(t *testing.T) {
	t.Parallel()
	term1 := map[string]int{"a": 5}
	term2 := map[string]int{"b": 3}
	sizes := map[string]int64{"a": 1000, "b": 1000}
	results := rankBM25([]map[string]int{term1, term2}, sizes, 100, queryOpOr, DefaultRankAlpha)

	if len(results) != 2 {
		t.Fatalf("expected 2 results for OR, got %d", len(results))
	}
}

func TestRankBM25TFSummedAcrossTerms(t *testing.T) {
	t.Parallel()
	term1 := map[string]int{"a": 3}
	term2 := map[string]int{"a": 4}
	sizes := map[string]int64{"a": 1000}
	results := rankBM25([]map[string]int{term1, term2}, sizes, 100, queryOpAnd, DefaultRankAlpha)

	if len(results) != 1 || results[0].TF != 7 {
		t.Fatalf("expected TF=7 for 'a', got %v", results)
	}
}

func TestBuildCandidateSetAND(t *testing.T) {
	t.Parallel()
	hitMaps := []map[string]int{
		{"a": 1, "b": 2},
		{"b": 3, "c": 1},
	}
	candidates := buildCandidateSet(hitMaps, queryOpAnd)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate (b), got %v", candidates)
	}
	if _, ok := candidates["b"]; !ok {
		t.Fatal("expected 'b' in AND candidates")
	}
}

func TestBuildCandidateSetOR(t *testing.T) {
	t.Parallel()
	hitMaps := []map[string]int{
		{"a": 1},
		{"b": 3},
	}
	candidates := buildCandidateSet(hitMaps, queryOpOr)
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates (a, b), got %v", candidates)
	}
}

func TestSortRankedResultsTieBreaksByTF(t *testing.T) {
	t.Parallel()
	results := []RankedResult{
		{FileID: "a", Score: 1.25, TF: 2},
		{FileID: "b", Score: 1.25, TF: 5},
	}

	sortRankedResults(results, DefaultRankAlpha)

	if results[0].FileID != "b" {
		t.Fatalf("expected higher-TF result first when scores tie, got %q", results[0].FileID)
	}
}

func TestSortRankedResultsTieBreaksByFileID(t *testing.T) {
	t.Parallel()
	results := []RankedResult{
		{FileID: "z", Score: 1.25, TF: 5},
		{FileID: "a", Score: 1.25, TF: 5},
	}

	sortRankedResults(results, DefaultRankAlpha)

	if results[0].FileID != "a" {
		t.Fatalf("expected lexicographically smaller file ID first on full tie, got %q", results[0].FileID)
	}
}

func TestSortRankedResultsHybridCanPreferHigherTF(t *testing.T) {
	t.Parallel()
	results := []RankedResult{
		{FileID: "low-tf", Score: 1.00, TF: 1},
		{FileID: "high-tf", Score: 0.90, TF: 100},
	}

	sortRankedResults(results, 0.35)

	if results[0].FileID != "high-tf" {
		t.Fatalf("expected higher TF to win when BM25 scores are close, got %q", results[0].FileID)
	}
}

func TestSortRankedResultsNegativeAlphaCountsAsZero(t *testing.T) {
	t.Parallel()
	results := []RankedResult{
		{FileID: "high-tf", Score: 0.90, TF: 100},
		{FileID: "low-tf", Score: 1.00, TF: 1},
	}

	sortRankedResults(results, -1)

	if results[0].FileID != "low-tf" {
		t.Fatalf("expected BM25 alone to rank with a negative alpha, got %q first", results[0].FileID)
	}
}

func TestFTSBoostKeepsRankAlpha(t *testing.T) {
	t.Parallel()
	results := []RankedResult{
		{FileID: "low-tf", Score: 1.00, TF: 1},
		{FileID: "high-tf", Score: 0.90, TF: 100},
	}
	scores := map[string]float64{"low-tf": 0.1, "high-tf": 0.1}

	ranked := applyFTSBoost(results, scores, 0.35)

	if ranked[0].FileID != "high-tf" {
		t.Fatalf("full-text boost ignored --rank-alpha: got %q first", ranked[0].FileID)
	}
}
