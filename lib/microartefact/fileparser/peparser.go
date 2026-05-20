package fileparser

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	saferpe "github.com/saferwall/pe"
)

func parsePEContent(content []byte) ([]byte, bool) {
	if len(content) == 0 {
		return nil, false
	}

	tmpFile, err := os.CreateTemp("", "indicer-pe-*.bin")
	if err != nil {
		return nil, false
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	defer tmpFile.Close()

	if _, err := tmpFile.Write(content); err != nil {
		return nil, false
	}

	parsed, err := saferpe.New(tmpPath, &saferpe.Options{})
	if err != nil {
		return nil, false
	}
	if err := parsed.Parse(); err != nil {
		return nil, false
	}

	return combineParsedPayload(extractPrintableText(content), buildPEMetadataText(parsed)), true
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
