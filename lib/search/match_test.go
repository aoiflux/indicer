package search

import (
	"math/rand/v2"
	"testing"
	"unicode/utf8"
)

// countInPieces feeds data to a term's state in pieces cut at cuts, as the
// spans of an object arrive, and returns the matches.
func countInPieces(term string, data []byte, cuts []int) int64 {
	t := &termState{m: newMatcher(term)}
	prev := 0
	for _, c := range cuts {
		c = min(max(c, prev), len(data))
		t.feed(data[prev:c])
		prev = c
	}
	t.feed(data[prev:])
	return t.count
}

func TestMatcherCounts(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		term, data string
		want       int64
	}{
		{"ab", "xxABabAbx", 3},
		{"aa", "aaaaa", 2},             // leftmost, without overlap
		{"école", "école ÉCOLE", 2},    // Unicode folding
		{"ÉCOLE", "Ecole école", 1},    // no accent folding
		{"\xffq", "\xffQ\xffq\xfe", 2}, // not UTF-8: bytewise, ASCII folded
		{"éa", "\xc3\xa9A\xc3", 1},     // a rune cut off at the end
		{"ab", "a", 0},
	} {
		if got := countInPieces(c.term, []byte(c.data), nil); got != c.want {
			t.Errorf("%q in %q: %d, want %d", c.term, c.data, got, c.want)
		}
	}
}

// TestMatcherStreamsAcrossCuts (SRC-1/2): the matches of a stream do not
// depend on where it is cut into spans, and for a bytewise term they are
// what counting the whole stream finds.
func TestMatcherStreamsAcrossCuts(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	alphabet := []string{"a", "b", "A", "B", "é", "É", ".", "\xc3"}
	for _, term := range []string{"ab", "aba", "bAb", "é", "éa", "aÉb", "\xc3a"} {
		for range 300 {
			var data []byte
			for range rng.IntN(64) {
				data = append(data, alphabet[rng.IntN(len(alphabet))]...)
			}
			whole := countInPieces(term, data, nil)
			if isASCII([]byte(term)) || !utf8.ValidString(term) {
				if want := foldCount(term, data); whole != want {
					t.Fatalf("%q in %q: %d, want %d", term, data, whole, want)
				}
			}
			cuts := make([]int, rng.IntN(6))
			for i := range cuts {
				cuts[i] = rng.IntN(len(data) + 1)
			}
			if got := countInPieces(term, data, sortedInts(cuts)); got != whole {
				t.Fatalf("%q in %q cut at %v: %d, want %d", term, data, cuts, got, whole)
			}
		}
	}
}

func sortedInts(s []int) []int {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}

func FuzzStreamCount(f *testing.F) {
	f.Add([]byte("xxABabAbx"), "ab", uint16(3), uint16(5))
	f.Add([]byte("école ÉCOLE"), "école", uint16(1), uint16(8))
	f.Add([]byte("\xffQ\xffq\xfe"), "\xffq", uint16(1), uint16(2))
	f.Fuzz(func(t *testing.T, data []byte, term string, c1, c2 uint16) {
		if len(term) < 2 || len(term) > 64 || len(data) > 1<<12 {
			t.Skip()
		}
		whole := countInPieces(term, data, nil)
		if isASCII([]byte(term)) || !utf8.ValidString(term) {
			if want := foldCount(term, data); whole != want {
				t.Fatalf("%q in %q: %d, want %d", term, data, whole, want)
			}
		}
		cuts := sortedInts([]int{int(c1) % (len(data) + 1), int(c2) % (len(data) + 1)})
		if got := countInPieces(term, data, cuts); got != whole {
			t.Fatalf("%q in %q cut at %v: %d, want %d", term, data, cuts, got, whole)
		}
	})
}
