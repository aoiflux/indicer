package detectors

import (
	"bytes"
	"debug/pe"
	"fmt"
	"strconv"
	"time"

	"indicer/lib/microartefact/model"
)

type PEMetadataDetector struct{}

func (d PEMetadataDetector) Name() string {
	return "pe-metadata"
}

func (d PEMetadataDetector) Detect(_ model.FileRecord, content []byte) ([]model.Artefact, error) {
	reader := bytes.NewReader(content)
	peFile, err := pe.NewFile(reader)
	if err != nil {
		return nil, nil
	}
	defer peFile.Close()

	results := make([]model.Artefact, 0, 32)

	ts := peFile.FileHeader.TimeDateStamp
	if ts != 0 {
		timeValue := time.Unix(int64(ts), 0).UTC().Format(time.RFC3339)
		results = append(results, model.Artefact{
			Kind:       "pe_timestamp",
			Detector:   d.Name(),
			Value:      timeValue,
			Summary:    "PE compile timestamp=" + timeValue,
			Confidence: 0.95,
		})
	}

	for _, section := range peFile.Sections {
		start := int64(section.Offset)
		end := start + int64(section.Size)
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		entropy := estimateEntropy(content[start:end])
		value := fmt.Sprintf("%s size=%d entropy=%.2f", section.Name, section.Size, entropy)
		results = append(results, model.Artefact{
			Kind:       "pe_section",
			Detector:   d.Name(),
			Value:      value,
			Summary:    value,
			Confidence: 0.9,
			Span:       model.Span{Start: start, End: end},
			Attributes: map[string]string{
				"name":    section.Name,
				"size":    strconv.FormatUint(uint64(section.Size), 10),
				"entropy": fmt.Sprintf("%.4f", entropy),
			},
		})
	}

	imports, importErr := peFile.ImportedSymbols()
	if importErr == nil {
		for _, symbol := range imports {
			results = append(results, model.Artefact{
				Kind:       "pe_import",
				Detector:   d.Name(),
				Value:      symbol,
				Summary:    truncate(symbol, 96),
				Confidence: 0.9,
			})
		}
	}

	return results, nil
}
