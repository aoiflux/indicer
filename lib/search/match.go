package search

import (
	"bytes"
	"unicode"
	"unicode/utf8"
)

// A matcher counts the occurrences of one query term in a byte stream,
// ignoring case: leftmost first, without overlap, as bytes.Count does. An
// ASCII term, or one that is not valid UTF-8, is matched byte by byte with
// ASCII letters folded; any other term is compared rune by rune after
// unicode.ToLower, so a rune split across two chunks still matches.
type matcher struct {
	bytewise bool
	query    []byte // the bytewise term, ASCII letters lower-cased
	runes    []rune // the term's runes, lower-cased, when it is UTF-8 and not ASCII
	window   int    // the most bytes one match can span
	c0, c1   byte   // the ASCII term's first byte, in both cases
}

func newMatcher(term string) *matcher {
	if isASCII([]byte(term)) || !utf8.ValidString(term) {
		q := make([]byte, len(term))
		for i := range q {
			q[i] = toLowerASCII(term[i])
		}
		c1 := q[0]
		if c1 >= 'a' && c1 <= 'z' {
			c1 -= 'a' - 'A'
		}
		return &matcher{bytewise: true, query: q, window: len(q), c0: q[0], c1: c1}
	}
	runes := []rune(term)
	for i, r := range runes {
		runes[i] = unicode.ToLower(r)
	}
	return &matcher{runes: runes, window: utf8.UTFMax * len(runes)}
}

// scan counts the matches that start at positions from to limit-1 of data,
// as far as data decides them. It returns the count and the first position
// it did not decide: limit or past it when every position was decided, or
// the position from which data is too short to tell, every byte from there
// on fewer than the matcher's window.
func (m *matcher) scan(data []byte, from, limit int) (int64, int) {
	if m.bytewise {
		return m.scanBytes(data, from, limit)
	}
	var count int64
	p := from
	for p < limit {
		n, ok, more := m.matchRunes(data, p)
		if more {
			break
		}
		if ok {
			count++
			p += n
			continue
		}
		p++
	}
	return count, p
}

func (m *matcher) scanBytes(data []byte, from, limit int) (int64, int) {
	n := len(m.query)
	// A match can start no later than last; positions past it are undecided.
	end := min(limit, len(data)-n+1)
	find := func(c byte, at int) int {
		if at >= end {
			return -1
		}
		if i := bytes.IndexByte(data[at:end], c); i >= 0 {
			return at + i
		}
		return -1
	}
	var count int64
	p := from
	// The next position of each case of the first byte; each is searched
	// again only once p passes it, so a scan is linear.
	n0, n1 := find(m.c0, p), -1
	if m.c1 != m.c0 {
		n1 = find(m.c1, p)
	}
	for p < end {
		if n0 >= 0 && n0 < p {
			n0 = find(m.c0, p)
		}
		if n1 >= 0 && n1 < p {
			n1 = find(m.c1, p)
		}
		next := n0
		if next < 0 || (n1 >= 0 && n1 < next) {
			next = n1
		}
		if next < 0 {
			p = end
			break
		}
		p = next
		if foldEqualASCII(m.query, data[p:p+n]) {
			count++
			p += n
			continue
		}
		p++
	}
	return count, max(p, from)
}

// matchRunes reports whether the term matches data at p, and the bytes the
// match spans; more is set when data ends before that can be told.
func (m *matcher) matchRunes(data []byte, p int) (n int, ok, more bool) {
	i := p
	for _, want := range m.runes {
		if i >= len(data) {
			return 0, false, true
		}
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size <= 1 {
			// An invalid byte matches no rune of a valid term; the start of
			// a rune the next span completes is not known yet.
			if !utf8.FullRune(data[i:]) {
				return 0, false, true
			}
			return 0, false, false
		}
		if unicode.ToLower(r) != want {
			return 0, false, false
		}
		i += size
	}
	return i - p, true, false
}

func isASCII(data []byte) bool {
	for _, c := range data {
		if c > 0x7f {
			return false
		}
	}
	return true
}

func toLowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

func foldEqualASCII(lower, data []byte) bool {
	for i, c := range lower {
		if toLowerASCII(data[i]) != c {
			return false
		}
	}
	return true
}

// termState follows one term through one object's bytes. The bytes at the
// end of a span that could still start a match (fewer than the window) are
// carried to the next span, so a match across a chunk boundary is counted
// once, and one that runs past the object's end is not counted.
type termState struct {
	m       *matcher
	carry   []byte
	count   int64
	scratch []byte
}

// junction decides the positions in the carry, with head (the next span's
// first bytes) after it. It returns the matches found, and where in the span
// the scan goes on. ok is false when head is too short to decide them all;
// next is then the first undecided position of the carry and head, which
// junction leaves in t.scratch.
func (t *termState) junction(head []byte) (n int64, start, next int, ok bool) {
	c := len(t.carry)
	if c == 0 {
		return 0, 0, 0, true
	}
	j := append(append(t.scratch[:0], t.carry...), head[:min(len(head), t.m.window)]...)
	t.scratch = j
	n, next = t.m.scan(j, 0, c)
	if next < c {
		return n, 0, next, false
	}
	return n, next - c, next, true
}

// feed counts the matches data, the object's next bytes, completes or holds.
// It returns where the span's own scan started, and that scan's matches and
// end, or start -1 when data was too short to be scanned on its own.
func (t *termState) feed(data []byte) (start int, interior int64, next int) {
	jn, start, jnext, ok := t.junction(data)
	t.count += jn
	if !ok {
		// data is shorter than a match: all of it is still undecided.
		t.carry = append(t.carry[:0], t.scratch[jnext:]...)
		return -1, 0, 0
	}
	interior, next = t.m.scan(data, start, len(data))
	t.count += interior
	t.carry = append(t.carry[:0], data[next:]...)
	return start, interior, next
}
