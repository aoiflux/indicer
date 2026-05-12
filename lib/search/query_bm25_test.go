package search

import "testing"

// --- query parser tests ---

func TestParseQuerySingleTerm(t *testing.T) {
	pq := parseQuery("foo")
	if len(pq.terms) != 1 || pq.terms[0] != "foo" {
		t.Fatalf("expected [foo], got %v", pq.terms)
	}
	if pq.op != queryOpAnd {
		t.Fatal("expected AND operator for single term")
	}
}

func TestParseQueryAndMultiWord(t *testing.T) {
	pq := parseQuery("foo bar")
	if len(pq.terms) != 2 || pq.terms[0] != "foo" || pq.terms[1] != "bar" {
		t.Fatalf("expected [foo bar], got %v", pq.terms)
	}
	if pq.op != queryOpAnd {
		t.Fatal("expected AND operator for multi-word")
	}
}

func TestParseQueryOrOperator(t *testing.T) {
	pq := parseQuery("foo|bar")
	if len(pq.terms) != 2 || pq.terms[0] != "foo" || pq.terms[1] != "bar" {
		t.Fatalf("expected [foo bar], got %v", pq.terms)
	}
	if pq.op != queryOpOr {
		t.Fatal("expected OR operator for pipe-separated terms")
	}
}

func TestParseQueryPhraseTerm(t *testing.T) {
	pq := parseQuery(`"foo bar"`)
	if len(pq.terms) != 1 || pq.terms[0] != "foo bar" {
		t.Fatalf("expected single phrase term [foo bar], got %v", pq.terms)
	}
	if pq.op != queryOpAnd {
		t.Fatal("expected AND operator for phrase")
	}
}

func TestParseQueryNormalizesCase(t *testing.T) {
	pq := parseQuery("FOO BAR")
	for _, term := range pq.terms {
		for _, r := range term {
			if r >= 'A' && r <= 'Z' {
				t.Fatalf("expected lowercase terms, got %v", pq.terms)
			}
		}
	}
}

func TestValidateParsedQueryRejectsShortTerm(t *testing.T) {
	pq := parsedQuery{terms: []string{"a"}, op: queryOpAnd}
	if err := validateParsedQuery(pq); err == nil {
		t.Fatal("expected error for single-char term, got nil")
	}
}

func TestValidateParsedQueryRejectsEmpty(t *testing.T) {
	pq := parsedQuery{terms: nil, op: queryOpAnd}
	if err := validateParsedQuery(pq); err == nil {
		t.Fatal("expected error for empty terms, got nil")
	}
}

// --- BM25 tests ---

func TestRankBM25ReturnsNilForEmptyHits(t *testing.T) {
	result := rankBM25([]map[string]int{{}}, map[string]int64{}, 10, queryOpAnd)
	if result != nil {
		t.Fatalf("expected nil for empty hits, got %v", result)
	}
}

func TestRankBM25ReturnsNilForZeroTotalDocs(t *testing.T) {
	hits := map[string]int{"a": 5}
	result := rankBM25([]map[string]int{hits}, map[string]int64{"a": 100}, 0, queryOpAnd)
	if result != nil {
		t.Fatalf("expected nil for zero totalDocs, got %v", result)
	}
}

func TestRankBM25ScoresDescending(t *testing.T) {
	// doc "a" has more occurrences than doc "b" with same size, expect higher score
	hits := map[string]int{"a": 10, "b": 1}
	sizes := map[string]int64{"a": 1000, "b": 1000}
	results := rankBM25([]map[string]int{hits}, sizes, 100, queryOpAnd)
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
	term1 := map[string]int{"a": 5, "b": 3}
	term2 := map[string]int{"b": 2, "c": 1}
	sizes := map[string]int64{"a": 1000, "b": 1000, "c": 1000}
	results := rankBM25([]map[string]int{term1, term2}, sizes, 100, queryOpAnd)

	// AND: only "b" appears in both
	if len(results) != 1 || results[0].FileID != "b" {
		t.Fatalf("expected only 'b' in AND results, got %v", results)
	}
}

func TestRankBM25ORUnion(t *testing.T) {
	term1 := map[string]int{"a": 5}
	term2 := map[string]int{"b": 3}
	sizes := map[string]int64{"a": 1000, "b": 1000}
	results := rankBM25([]map[string]int{term1, term2}, sizes, 100, queryOpOr)

	if len(results) != 2 {
		t.Fatalf("expected 2 results for OR, got %d", len(results))
	}
}

func TestRankBM25TFSummedAcrossTerms(t *testing.T) {
	term1 := map[string]int{"a": 3}
	term2 := map[string]int{"a": 4}
	sizes := map[string]int64{"a": 1000}
	results := rankBM25([]map[string]int{term1, term2}, sizes, 100, queryOpAnd)

	if len(results) != 1 || results[0].TF != 7 {
		t.Fatalf("expected TF=7 for 'a', got %v", results)
	}
}

func TestBuildCandidateSetAND(t *testing.T) {
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
	results := []RankedResult{
		{FileID: "a", Score: 1.25, TF: 2},
		{FileID: "b", Score: 1.25, TF: 5},
	}

	sortRankedResults(results)

	if results[0].FileID != "b" {
		t.Fatalf("expected higher-TF result first when scores tie, got %q", results[0].FileID)
	}
}

func TestSortRankedResultsTieBreaksByFileID(t *testing.T) {
	results := []RankedResult{
		{FileID: "z", Score: 1.25, TF: 5},
		{FileID: "a", Score: 1.25, TF: 5},
	}

	sortRankedResults(results)

	if results[0].FileID != "a" {
		t.Fatalf("expected lexicographically smaller file ID first on full tie, got %q", results[0].FileID)
	}
}

func TestSortRankedResultsHybridCanPreferHigherTF(t *testing.T) {
	oldAlpha := GetOccurrenceBoostAlpha()
	t.Cleanup(func() { SetOccurrenceBoostAlpha(oldAlpha) })
	SetOccurrenceBoostAlpha(0.35)

	results := []RankedResult{
		{FileID: "low-tf", Score: 1.00, TF: 1},
		{FileID: "high-tf", Score: 0.90, TF: 100},
	}

	sortRankedResults(results)

	if results[0].FileID != "high-tf" {
		t.Fatalf("expected higher TF to win when BM25 scores are close, got %q", results[0].FileID)
	}
}

func TestSetOccurrenceBoostAlphaClampsNegativeToZero(t *testing.T) {
	oldAlpha := GetOccurrenceBoostAlpha()
	t.Cleanup(func() { SetOccurrenceBoostAlpha(oldAlpha) })

	SetOccurrenceBoostAlpha(-1)
	if got := GetOccurrenceBoostAlpha(); got != 0 {
		t.Fatalf("expected alpha to clamp to 0, got %v", got)
	}
}
