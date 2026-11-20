package fileparser

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"iter"
	"slices"

	pdffilter "github.com/pdfcpu/pdfcpu/pkg/filter"
)

// Limits on what a PDF's streams may inflate to. pdfcpu inflates every stream
// it reads in full, with no limit, so a small file whose streams decompress to
// gigabytes exhausted memory (GRA-14). The streams are measured first, within
// these limits, and a PDF over them is not given to pdfcpu.
const (
	pdfMaxStream = 16 << 20 // decoded bytes of one stream
	pdfMaxTotal  = 64 << 20 // decoded bytes of all streams
	pdfMaxRatio  = 200      // decoded over encoded bytes, once past pdfRatioFloorBytes
	pdfMaxText   = 16 << 20 // text extracted from the pages

	pdfRatioFloorBytes = 1 << 20
	// pdfDictWindow is how far before its stream keyword a stream's
	// dictionary is looked for.
	pdfDictWindow = 1024
)

// ErrPDFTooLarge is a PDF whose streams inflate past the limits.
var ErrPDFTooLarge = errors.New("pdf: streams inflate past the limits")

var (
	pdfStreamKeyword = []byte("stream")
	pdfEndstream     = []byte("endstream")
)

const pdfNameFilter = "Filter"

var pdfFilterNames = map[string]string{
	"FlateDecode": pdffilter.Flate, "Fl": pdffilter.Flate,
	"LZWDecode": pdffilter.LZW, "LZW": pdffilter.LZW,
	"RunLengthDecode": pdffilter.RunLength, "RL": pdffilter.RunLength,
	"ASCII85Decode": pdffilter.ASCII85, "A85": pdffilter.ASCII85,
	"ASCIIHexDecode": pdffilter.ASCIIHex, "AHx": pdffilter.ASCIIHex,
}

// pdfStream is one stream of a PDF as found by scanning its bytes: the names
// of its dictionary, decoded, and its encoded data.
type pdfStream struct {
	names []string
	data  []byte
}

// filters returns the filter chain s names that can expand it, and whether
// it names a filter outside that set.
func (s pdfStream) filters() (chain []string, other bool) {
	for _, n := range s.names {
		if f, ok := pdfFilterNames[n]; ok {
			chain = append(chain, f)
		}
	}
	return chain, len(chain) == 0 && slices.Contains(s.names, pdfNameFilter)
}

// pdfStreams yields the streams of a PDF in file order. A stream's dictionary
// is taken from its object header, or from pdfDictWindow bytes before its
// stream keyword when the header is further back.
func pdfStreams(content []byte) iter.Seq[pdfStream] {
	return func(yield func(pdfStream) bool) {
		rest := content
		for {
			i := bytes.Index(rest, pdfStreamKeyword)
			if i < 0 {
				return
			}
			if i >= len(pdfEndPrefix) && bytes.Equal(rest[i-len(pdfEndPrefix):i], pdfEndPrefix) {
				rest = rest[i+len(pdfStreamKeyword):]
				continue
			}
			dict := rest[max(0, i-pdfDictWindow):i]
			if j := bytes.LastIndex(dict, pdfObjKeyword); j >= 0 {
				dict = dict[j:]
			}
			body := rest[i+len(pdfStreamKeyword):]
			if len(body) > 0 && body[0] == '\r' {
				body = body[1:]
			}
			if len(body) > 0 && body[0] == '\n' {
				body = body[1:]
			}
			end := bytes.Index(body, pdfEndstream)
			if end < 0 {
				end = len(body)
			}
			rest = body[end:]
			if !yield(pdfStream{names: pdfNames(dict), data: body[:end]}) {
				return
			}
		}
	}
}

// checkPDFInflation decodes every stream of a PDF whose dictionary names a
// filter that can expand it, stopping at the limits, and reports
// ErrPDFTooLarge when one is reached. Image filters are not decoded by text
// extraction, so their streams are not measured.
func checkPDFInflation(content []byte) error {
	var decoded, encoded int64
	for s := range pdfStreams(content) {
		chain, _ := s.filters()
		if len(chain) == 0 {
			continue
		}
		n := int64(len(decodeBounded(s.data, chain)))
		if n > pdfMaxStream {
			return fmt.Errorf("%w: a stream inflates past %d MiB", ErrPDFTooLarge, pdfMaxStream>>20)
		}
		decoded += n
		encoded += int64(len(s.data))
		if decoded > pdfMaxTotal {
			return fmt.Errorf("%w: its streams inflate past %d MiB", ErrPDFTooLarge, pdfMaxTotal>>20)
		}
	}
	if decoded > pdfRatioFloorBytes && decoded > pdfMaxRatio*encoded {
		return fmt.Errorf("%w: %d bytes of streams inflate to %d", ErrPDFTooLarge, encoded, decoded)
	}
	return nil
}

// decodeBounded decodes data through the filter chain, each stage stopping
// just past the per-stream limit. A stage that fails ends the chain with what
// it reached; pdfcpu meets the same failure when it reads the stream.
func decodeBounded(data []byte, chain []string) []byte {
	cur := data
	for _, name := range chain {
		f, err := pdffilter.NewFilter(name, nil)
		if err != nil {
			break
		}
		r, err := f.DecodeLength(bytes.NewReader(cur), pdfMaxStream+1)
		if err != nil {
			break
		}
		next, err := io.ReadAll(io.LimitReader(r, pdfMaxStream+1))
		if err != nil && len(next) == 0 {
			break
		}
		cur = next
		if int64(len(cur)) > pdfMaxStream {
			break
		}
	}
	return cur
}
