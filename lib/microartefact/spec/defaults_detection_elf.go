package spec

import parsingpkg "indicer/lib/microartefact/parsing"

func defaultELFDetectionRules() []DetectionRule {
	return []DetectionRule{
		{
			ID:           "elf.url",
			DetectorName: "url-pattern",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindELF},
			Enabled:      true,
		},
		{
			ID:           "elf.ioc",
			DetectorName: "ioc",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindELF},
			Enabled:      true,
		},
		{
			ID:           "elf.keyvalue",
			DetectorName: "key-value",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindELF},
			Enabled:      true,
		},
		{
			ID:           "elf.logline",
			DetectorName: "log-line",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindELF},
			Enabled:      true,
		},
		{
			ID:           "elf.service",
			DetectorName: "service-artefact",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindELF},
			Enabled:      true,
		},
	}
}
