package fileparser

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"

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
)

// ErrPDFTooLarge is a PDF whose streams inflate past the limits.
var ErrPDFTooLarge = errors.New("pdf: streams inflate past the limits")

var (
	pdfStreamKeyword = []byte("stream")
	pdfEndstream     = []byte("endstream")
	pdfFilterPattern = regexp.MustCompile(`/(FlateDecode|Fl|LZWDecode|LZW|RunLengthDecode|RL|ASCII85Decode|A85|ASCIIHexDecode|AHx)\b`)
)

var pdfFilterNames = map[string]string{
	"FlateDecode": pdffilter.Flate, "Fl": pdffilter.Flate,
	"LZWDecode": pdffilter.LZW, "LZW": pdffilter.LZW,
	"RunLengthDecode": pdffilter.RunLength, "RL": pdffilter.RunLength,
	"ASCII85Decode": pdffilter.ASCII85, "A85": pdffilter.ASCII85,
	"ASCIIHexDecode": pdffilter.ASCIIHex, "AHx": pdffilter.ASCIIHex,
}

// checkPDFInflation decodes every stream of a PDF whose dictionary names a
// filter that can expand it, stopping at the limits, and reports
// ErrPDFTooLarge when one is reached. Image filters are not decoded by text
// extraction, so their streams are not measured.
func checkPDFInflation(content []byte) error {
	var decoded, encoded int64
	rest := content
	for {
		i := bytes.Index(rest, pdfStreamKeyword)
		if i < 0 {
			break
		}
		if i >= 3 && bytes.Equal(rest[i-3:i], []byte("end")) {
			rest = rest[i+len(pdfStreamKeyword):]
			continue
		}
		dict := rest[max(0, i-1024):i]
		if j := bytes.LastIndex(dict, []byte("<<")); j >= 0 {
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
		data := body[:end]
		rest = body[end:]

		var chain []string
		for _, m := range pdfFilterPattern.FindAllSubmatch(dict, -1) {
			chain = append(chain, pdfFilterNames[string(m[1])])
		}
		if len(chain) == 0 {
			continue
		}
		n := decodedSize(data, chain)
		if n > pdfMaxStream {
			return fmt.Errorf("%w: a stream inflates past %d MiB", ErrPDFTooLarge, pdfMaxStream>>20)
		}
		decoded += n
		encoded += int64(len(data))
		if decoded > pdfMaxTotal {
			return fmt.Errorf("%w: its streams inflate past %d MiB", ErrPDFTooLarge, pdfMaxTotal>>20)
		}
	}
	if decoded > pdfRatioFloorBytes && decoded > pdfMaxRatio*encoded {
		return fmt.Errorf("%w: %d bytes of streams inflate to %d", ErrPDFTooLarge, encoded, decoded)
	}
	return nil
}

// decodedSize decodes data through the filter chain, each stage stopping just
// past the per-stream limit. A stage that fails ends the chain with the size
// reached; pdfcpu meets the same failure when it reads the stream.
func decodedSize(data []byte, chain []string) int64 {
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
	return int64(len(cur))
}
