package fileparser

import (
	"bytes"
	"errors"
	"fmt"
	"math"
)

// ErrPDFCount is a PDF whose declared counts would have pdfcpu allocate past
// the limits before it reads any data.
var ErrPDFCount = errors.New("pdf: declared counts run past the limits")

const (
	pdfNameSize    = "Size"
	pdfNameIndex   = "Index"
	pdfNameFirst   = "First"
	pdfNameColumns = "Columns"
	pdfNameColors  = "Colors"
	// pdfMaxObjects bounds the objects a PDF may declare; far more than any
	// document text extraction is for.
	pdfMaxObjects = 1 << 22
	// pdfMaxSampleBits is the widest sample pdfcpu's predictors accept.
	pdfMaxSampleBits = 16
	pdfBitsPerByte   = 8
)

// checkPDFCounts refuses a PDF whose declared counts would make pdfcpu
// allocate past the limits, which ends the process: pdfcpu lists every object
// number an xref stream's /Size or /Index names and walks every number below
// the trailer's /Size, decodes an object stream's first /First bytes into a
// buffer of that size, and holds two predictor rows of /Columns samples of
// /Colors components. A value is taken at its largest anywhere in the file.
// pdfcpu reads none of these through a reference, so references are skipped.
func checkPDFCounts(content []byte) error {
	columns, colors := int64(1), int64(1) // pdfcpu's defaults
	for i := 0; ; i++ {
		j := bytes.IndexByte(content[i:], '/')
		if j < 0 {
			break
		}
		i += j
		name, end := pdfName(content, i)
		switch name {
		case pdfNameSize:
			if v, ok := pdfDirectInt(content, end); ok && v > pdfMaxObjects {
				return fmt.Errorf("%w: /Size declares %d objects, more than %d", ErrPDFCount, v, pdfMaxObjects)
			}
		case pdfNameIndex:
			if n := pdfIndexCount(content, end); n > pdfMaxObjects {
				return fmt.Errorf("%w: /Index declares more than %d objects", ErrPDFCount, pdfMaxObjects)
			}
		case pdfNameFirst:
			if v, ok := pdfDirectInt(content, end); ok && v > pdfMaxStream {
				return fmt.Errorf("%w: an object stream's /First is past %d MiB", ErrPDFCount, pdfMaxStream>>20)
			}
		case pdfNameColumns:
			if v, ok := pdfDirectInt(content, end); ok {
				columns = max(columns, pdfMagnitude(v))
			}
		case pdfNameColors:
			if v, ok := pdfDirectInt(content, end); ok {
				colors = max(colors, pdfMagnitude(v))
			}
		}
		i = end - 1
	}
	if columns > pdfMaxStream || colors > pdfMaxStream ||
		columns*colors*pdfMaxSampleBits/pdfBitsPerByte > pdfMaxStream {
		return fmt.Errorf("%w: a predictor row of %d columns of %d colours is past %d MiB", ErrPDFCount, columns, colors, pdfMaxStream>>20)
	}
	return nil
}

// pdfDirectInt reads an integer at b[i:] that is not an indirect reference.
func pdfDirectInt(b []byte, i int) (int64, bool) {
	v, ok := pdfLengthValue(b, i)
	return v.value, ok && !v.ref
}

// pdfIndexCount returns the objects an /Index array at b[i:] names: the sum
// of its counts, every second integer, up to the first element that is not an
// integer, where pdfcpu stops. It stops counting once past pdfMaxObjects.
func pdfIndexCount(b []byte, i int) int64 {
	i = pdfSkipSpace(b, i)
	if i >= len(b) || b[i] != '[' {
		return 0
	}
	i++
	var total int64
	for k := 0; total <= pdfMaxObjects; k++ {
		v, end, ok := pdfInt(b, pdfSkipSpace(b, i))
		if !ok {
			break
		}
		if k%2 == 1 {
			total += min(max(v, 0), pdfMaxObjects+1)
		}
		i = end
	}
	return total
}

// pdfMagnitude returns |v|, saturating at math.MaxInt64: pdfcpu multiplies
// predictor parameters without checking their sign.
func pdfMagnitude(v int64) int64 {
	switch {
	case v >= 0:
		return v
	case v == math.MinInt64:
		return math.MaxInt64
	default:
		return -v
	}
}
