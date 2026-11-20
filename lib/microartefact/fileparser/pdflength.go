package fileparser

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// ErrPDFLength is a PDF whose streams declare more bytes than it can hold.
var ErrPDFLength = errors.New("pdf: stream lengths run past the file")

const (
	pdfWhitespace = "\x00\t\n\f\r "
	pdfDelimiters = "()<>[]{}/%"
	pdfNameLength = "Length"
	pdfNameObjStm = "ObjStm"
	pdfNameCrypt  = "Encrypt"
	// pdfMaxNameRaw is the longest spelling of a name decoded here: every
	// byte of the longest name looked for written as #xx.
	pdfMaxNameRaw = 3 * len("RunLengthDecode")
	pdfNameEscape = '#'
	pdfHexDigits  = 2
)

var (
	pdfObjKeyword = []byte("obj")
	pdfEndPrefix  = []byte("end")
)

// pdfLength is the value of one /Length entry: an integer, or the object
// number of the integer object it refers to.
type pdfLength struct {
	value int64
	ref   bool
}

// checkPDFLengths refuses a PDF whose streams' declared lengths add up past
// the file plus pdfMaxTotal. pdfcpu allocates a stream's /Length before it
// reads the stream, so a few bytes declaring terabytes ended the process; the
// margin leaves room for a truncated file's last stream.
//
// A length given by reference counts as the largest value its object could
// have: any plain object of that number, or any integer in an object stream,
// where a later definition can shadow the plain one. An object stream that
// cannot be read here, encrypted or under another filter, makes such a length
// unbounded.
func checkPDFLengths(content []byte) error {
	var lengths []pdfLength
	objStms, encrypted := 0, false
	for i := 0; ; i++ {
		j := bytes.IndexByte(content[i:], '/')
		if j < 0 {
			break
		}
		i += j
		name, end := pdfName(content, i)
		switch name {
		case pdfNameObjStm:
			objStms++
		case pdfNameCrypt:
			encrypted = true
		case pdfNameLength:
			if l, ok := pdfLengthValue(content, end); ok {
				lengths = append(lengths, l)
			}
		}
		i = end - 1
	}

	limit := int64(len(content)) + pdfMaxTotal
	ints, unnumbered := pdfIntegerObjects(content)
	objStmMax := int64(0)
	if objStms > 0 && slices.ContainsFunc(lengths, func(l pdfLength) bool { return l.ref }) {
		objStmMax = pdfObjStmMax(content, objStms, encrypted)
	}
	var total int64
	for _, l := range lengths {
		n := l.value
		if l.ref {
			target, ok := ints[l.value]
			if !ok {
				target = unnumbered
			}
			n = max(n, target, objStmMax)
		}
		total += min(max(n, 0), limit+1)
		if total > limit {
			return fmt.Errorf("%w: its streams declare more than %d bytes", ErrPDFLength, limit)
		}
	}
	return nil
}

// pdfLengthValue reads the value of a /Length entry at b[i:].
func pdfLengthValue(b []byte, i int) (pdfLength, bool) {
	v, end, ok := pdfInt(b, pdfSkipSpace(b, i))
	if !ok {
		return pdfLength{}, false
	}
	gen := pdfSkipSpace(b, end)
	_, genEnd, ok := pdfInt(b, gen)
	if !ok || gen == end {
		return pdfLength{value: v}, true
	}
	r := pdfSkipSpace(b, genEnd)
	return pdfLength{value: v, ref: r > genEnd && r < len(b) && b[r] == 'R'}, true
}

// pdfObjStmMax returns the largest integer in the PDF's object streams, or
// math.MaxInt64 when one cannot be read: the file is encrypted, a stream
// names a filter not decoded here, or fewer object streams were found than
// the file names.
func pdfObjStmMax(content []byte, named int, encrypted bool) int64 {
	if encrypted {
		return math.MaxInt64
	}
	var m int64
	found := 0
	for s := range pdfStreams(content) {
		if !slices.Contains(s.names, pdfNameObjStm) {
			continue
		}
		found++
		chain, other := s.filters()
		if other {
			return math.MaxInt64
		}
		data := s.data
		if len(chain) > 0 {
			data = decodeBounded(data, chain)
		}
		m = max(m, pdfMaxInteger(data))
	}
	if found != named {
		return math.MaxInt64
	}
	return m
}

// pdfIntegerObjects finds every top-level object whose value starts with an
// integer and returns the largest value for each object number, and the
// largest of those whose number could not be read.
func pdfIntegerObjects(b []byte) (ints map[int64]int64, unnumbered int64) {
	ints = map[int64]int64{}
	for i := 0; ; {
		j := bytes.Index(b[i:], pdfObjKeyword)
		if j < 0 {
			return ints, unnumbered
		}
		at := i + j
		i = at + len(pdfObjKeyword)
		if bytes.HasSuffix(b[:at], pdfEndPrefix) {
			continue
		}
		v, _, ok := pdfInt(b, pdfSkipSpace(b, i))
		if !ok {
			continue
		}
		nr, ok := pdfObjectNumber(b, at)
		if !ok {
			unnumbered = max(unnumbered, v)
			continue
		}
		ints[nr] = max(ints[nr], v)
	}
}

// pdfObjectNumber reads the object number of the "N G obj" header whose
// keyword starts at at.
func pdfObjectNumber(b []byte, at int) (int64, bool) {
	genEnd := pdfSkipSpaceBack(b, at)
	if genEnd == at {
		return 0, false
	}
	genStart, ok := pdfDigitsBack(b, genEnd)
	if !ok {
		return 0, false
	}
	nrEnd := pdfSkipSpaceBack(b, genStart)
	if nrEnd == genStart {
		return 0, false
	}
	nrStart, ok := pdfDigitsBack(b, nrEnd)
	if !ok {
		return 0, false
	}
	nr, err := strconv.ParseInt(string(b[nrStart:nrEnd]), 10, 64)
	return nr, err == nil
}

// pdfMaxInteger returns the largest run of digits in data, read as an
// integer.
func pdfMaxInteger(data []byte) int64 {
	var m int64
	for i := 0; i < len(data); {
		if !pdfIsDigit(data[i]) {
			i++
			continue
		}
		v, end, _ := pdfInt(data, i)
		m = max(m, v)
		i = end
	}
	return m
}

// pdfNames returns the names in b, decoded.
func pdfNames(b []byte) []string {
	var names []string
	for i := 0; ; i++ {
		j := bytes.IndexByte(b[i:], '/')
		if j < 0 {
			return names
		}
		i += j
		name, end := pdfName(b, i)
		if name != "" {
			names = append(names, name)
		}
		i = end - 1
	}
}

// pdfName reads the name starting with the '/' at b[i], decoding #xx
// escapes as pdfcpu does, and returns it with where it ends. A name longer
// than pdfMaxNameRaw comes back empty: none this file looks for is that long.
func pdfName(b []byte, i int) (string, int) {
	end := i + 1
	for end < len(b) && !pdfIsSpace(b[end]) && strings.IndexByte(pdfDelimiters, b[end]) < 0 {
		end++
	}
	raw := b[i+1 : end]
	if len(raw) > pdfMaxNameRaw {
		return "", end
	}
	if bytes.IndexByte(raw, pdfNameEscape) < 0 {
		return string(raw), end
	}
	out := make([]byte, 0, len(raw))
	for k := 0; k < len(raw); k++ {
		if raw[k] == pdfNameEscape && k+pdfHexDigits < len(raw) {
			if c, err := strconv.ParseUint(string(raw[k+1:k+1+pdfHexDigits]), 16, 8); err == nil {
				out = append(out, byte(c))
				k += pdfHexDigits
				continue
			}
		}
		out = append(out, raw[k])
	}
	return string(out), end
}

// pdfInt reads an integer at b[i:]. One too large for an int64 reads as
// math.MaxInt64, so it is past any limit.
func pdfInt(b []byte, i int) (v int64, end int, ok bool) {
	j := i
	if j < len(b) && (b[j] == '+' || b[j] == '-') {
		j++
	}
	k := j
	for k < len(b) && pdfIsDigit(b[k]) {
		k++
	}
	if k == j {
		return 0, i, false
	}
	v, err := strconv.ParseInt(string(b[i:k]), 10, 64)
	if err != nil {
		v = math.MaxInt64
	}
	return v, k, true
}

// pdfSkipSpace skips whitespace and comments from b[i].
func pdfSkipSpace(b []byte, i int) int {
	for i < len(b) {
		switch {
		case pdfIsSpace(b[i]):
			i++
		case b[i] == '%':
			for i < len(b) && b[i] != '\n' && b[i] != '\r' {
				i++
			}
		default:
			return i
		}
	}
	return i
}

// pdfSkipSpaceBack returns where the whitespace that ends at b[i-1] begins.
func pdfSkipSpaceBack(b []byte, i int) int {
	for i > 0 && pdfIsSpace(b[i-1]) {
		i--
	}
	return i
}

// pdfDigitsBack returns where the digits that end at b[i-1] begin.
func pdfDigitsBack(b []byte, i int) (int, bool) {
	start := i
	for start > 0 && pdfIsDigit(b[start-1]) {
		start--
	}
	return start, start < i
}

func pdfIsSpace(c byte) bool { return strings.IndexByte(pdfWhitespace, c) >= 0 }

func pdfIsDigit(c byte) bool { return c >= '0' && c <= '9' }
