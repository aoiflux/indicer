package fileparser

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	saferpe "github.com/saferwall/pe"
	pelog "github.com/saferwall/pe/log"
)

// parsePEContent parses a PE image from memory. It used to copy the evidence
// bytes to a temporary file and map it, and never closed the result, so the
// mapping and handle leaked and, on Windows, the temporary file with evidence
// content was left behind (GRA-15).
//
// The parsed file is not closed: for a file made by NewBytes, Close unmaps
// the data, which would unmap the caller's buffer (GRA-22). It holds nothing
// else.
func parsePEContent(content []byte) ([]byte, error) {
	if len(content) == 0 {
		return nil, errors.New("pe: empty")
	}
	parsed, err := saferpe.NewBytes(content, &saferpe.Options{
		// The library logs parse problems to stdout unless given a logger.
		Logger:                     pelog.NewStdLogger(io.Discard),
		DisableCertValidation:      true,
		DisableSignatureValidation: true,
	})
	if err != nil {
		return nil, fmt.Errorf("pe: %w", err)
	}
	if err := parsed.Parse(); err != nil {
		return nil, fmt.Errorf("pe: %w", err)
	}
	return combineParsedPayload(extractPrintableText(content), buildPEMetadataText(parsed)), nil
}

func buildPEMetadataText(parsed *saferpe.File) string {
	if parsed == nil {
		return ""
	}

	var b strings.Builder
	b.WriteString("__pe_metadata__\n")
	fmt.Fprintf(&b, "pe.section_count=%d\n", len(parsed.Sections))
	fmt.Fprintf(&b, "pe.import_dll_count=%d\n", len(parsed.Imports))

	if imphash, err := parsed.ImpHash(); err == nil && imphash != "" {
		fmt.Fprintf(&b, "pe.imphash=%s\n", imphash)
	}

	const maxSections = 64
	for i, section := range parsed.Sections {
		if i >= maxSections {
			fmt.Fprintf(&b, "pe.sections_truncated=%d\n", len(parsed.Sections)-maxSections)
			break
		}

		sectionName := bytes.TrimRight(section.Header.Name[:], "\x00")
		fmt.Fprintf(
			&b,
			"pe.section.%d=%s|vaddr=0x%x|vsize=%d|raw_size=%d\n",
			i,
			string(sectionName),
			section.Header.VirtualAddress,
			section.Header.VirtualSize,
			section.Header.SizeOfRawData,
		)
	}

	const maxImports = 256
	emittedImports := 0
	for _, imp := range parsed.Imports {
		dll := strings.TrimSpace(imp.Name)
		if dll == "" {
			dll = "unknown_dll"
		}

		for _, fn := range imp.Functions {
			if emittedImports >= maxImports {
				break
			}

			if fn.ByOrdinal {
				fmt.Fprintf(&b, "pe.import=%s!#%d\n", dll, fn.Ordinal)
			} else {
				name := strings.TrimSpace(fn.Name)
				if name == "" {
					name = "unknown"
				}
				fmt.Fprintf(&b, "pe.import=%s!%s\n", dll, name)
			}
			emittedImports++
		}

		if emittedImports >= maxImports {
			break
		}
	}
	if total := countPEImportFunctions(parsed.Imports); total > emittedImports {
		fmt.Fprintf(&b, "pe.imports_truncated=%d\n", total-emittedImports)
	}

	const maxAnomalies = 32
	for i, anomaly := range parsed.Anomalies {
		if i >= maxAnomalies {
			fmt.Fprintf(&b, "pe.anomalies_truncated=%d\n", len(parsed.Anomalies)-maxAnomalies)
			break
		}
		anomaly = strings.TrimSpace(anomaly)
		if anomaly == "" {
			continue
		}
		fmt.Fprintf(&b, "pe.anomaly=%s\n", anomaly)
	}

	return b.String()
}

func countPEImportFunctions(imports []saferpe.Import) int {
	total := 0
	for _, imp := range imports {
		total += len(imp.Functions)
	}
	return total
}
