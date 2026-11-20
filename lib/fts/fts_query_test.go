package fts

import (
	"strings"
	"testing"

	"github.com/blevesearch/bleve/v2/search/query"
)

func extract(pieces ...[]byte) string {
	x := textExtractor{remaining: MaxTextBytes}
	for _, p := range pieces {
		x.write(p)
	}
	return strings.Join(strings.Fields(x.out.String()), " ")
}

// TestTextExtractorAcrossPieces (SRC-7): text cut anywhere, inside a rune or
// a word, gives the same words as the whole; before, a piece ending inside a
// rune was taken for binary and dropped, and every piece ended a word.
func TestTextExtractorAcrossPieces(t *testing.T) {
	t.Parallel()
	text := []byte("Héllo wörld, ünïcode text: path/to/file_name.txt")
	whole := extract(text)
	if whole != "héllo wörld ünïcode text: path/to/file_name.txt" {
		t.Fatalf("whole text %q", whole)
	}
	for cut := 1; cut < len(text); cut++ {
		if got := extract(text[:cut], text[cut:]); got != whole {
			t.Fatalf("cut at %d: %q, want %q", cut, got, whole)
		}
	}
	if got := extract([]byte{0, 1, 2, 3, 0xff, 0xfe, 9, 0}); got != "" {
		t.Fatalf("binary bytes gave text %q", got)
	}
}

func TestTextExtractorStopsAtTheLimit(t *testing.T) {
	t.Parallel()
	x := textExtractor{remaining: 10}
	if full := x.write([]byte("abcdefghijklmnop")); !full {
		t.Fatal("the extractor wants more past its limit")
	}
	if got := x.out.String(); got != "abcdefghij" {
		t.Fatalf("text %q", got)
	}
}

func TestBuildQuery(t *testing.T) {
	t.Parallel()
	if q := buildQuery([]string{" ", ""}, QueryModeAnd); q != nil {
		t.Fatalf("empty terms built %T", q)
	}
	if _, ok := buildQuery([]string{"foo"}, QueryModeAnd).(*query.MatchPhraseQuery); !ok {
		t.Fatal("one term is not a phrase query")
	}
	if _, ok := buildQuery([]string{"foo", "bar baz"}, QueryModeAnd).(*query.ConjunctionQuery); !ok {
		t.Fatal("AND terms are not a conjunction")
	}
	if _, ok := buildQuery([]string{"foo", "bar"}, QueryModeOr).(*query.DisjunctionQuery); !ok {
		t.Fatal("OR terms are not a disjunction")
	}
}
