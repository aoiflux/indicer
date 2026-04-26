package fileparser

import (
	"bytes"
	"io"

	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	pdfcpu "github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	pdfmodel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

func parsePDFContent(content []byte) ([]byte, bool) {
	if len(content) == 0 {
		return nil, false
	}

	conf := pdfmodel.NewDefaultConfiguration()
	ctx, err := pdfapi.ReadValidateAndOptimize(bytes.NewReader(content), conf)
	if err != nil {
		return nil, false
	}

	buf := bytes.NewBuffer(nil)
	for page := 1; page <= ctx.PageCount; page++ {
		r, err := pdfcpu.ExtractPageContent(ctx, page)
		if err != nil {
			continue
		}
		if r == nil {
			continue
		}
		b, err := io.ReadAll(r)
		if err != nil {
			continue
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}

	if buf.Len() == 0 {
		return nil, false
	}

	return buf.Bytes(), true
}
