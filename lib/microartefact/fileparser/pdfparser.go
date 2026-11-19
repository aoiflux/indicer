package fileparser

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"

	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	pdfcpu "github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	pdfmodel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// pdfcpu writes a configuration directory under the user's configuration
// directory the first time a default configuration is made, unless told not
// to (GRA-23).
var disablePDFConfigDir sync.Once

// parsePDFContent extracts the text of a PDF's pages. The streams are measured
// against the inflation limits and their declared lengths against the file
// first (GRA-14); the file is read and validated,
// not optimised, which decoded every stream to deduplicate them; and the text
// kept is capped.
func parsePDFContent(content []byte) ([]byte, error) {
	if len(content) == 0 {
		return nil, errors.New("pdf: empty")
	}
	if err := checkPDFInflation(content); err != nil {
		return nil, err
	}
	if err := checkPDFLengths(content); err != nil {
		return nil, err
	}

	disablePDFConfigDir.Do(pdfapi.DisableConfigDir)
	conf := pdfmodel.NewDefaultConfiguration()
	ctx, err := pdfapi.ReadAndValidate(bytes.NewReader(content), conf)
	if err != nil {
		return nil, fmt.Errorf("pdf: %w", err)
	}

	buf := bytes.NewBuffer(nil)
	for page := 1; page <= ctx.PageCount && buf.Len() < pdfMaxText; page++ {
		r, err := pdfcpu.ExtractPageContent(ctx, page)
		if err != nil || r == nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(r, int64(pdfMaxText-buf.Len())))
		if err != nil {
			continue
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}

	if buf.Len() == 0 {
		return nil, errors.New("pdf: no page text")
	}
	return buf.Bytes(), nil
}
