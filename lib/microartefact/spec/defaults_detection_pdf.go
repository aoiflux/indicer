package spec

import parsingpkg "indicer/lib/microartefact/parsing"

func defaultPDFDetectionRules() []DetectionRule {
	return []DetectionRule{
		{
			ID:           "pdf.url",
			DetectorName: "url-pattern",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindPDF},
			Enabled:      true,
		},
		{
			ID:           "pdf.ioc",
			DetectorName: "ioc",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindPDF},
			Enabled:      true,
		},
		{
			ID:           "pdf.keyvalue",
			DetectorName: "key-value",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindPDF},
			Enabled:      true,
		},
		{
			ID:           "pdf.logline",
			DetectorName: "log-line",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindPDF},
			Enabled:      true,
		},
	}
}
