package spec

import parsingpkg "indicer/lib/microartefact/parsing"

func defaultPEDetectionRules() []DetectionRule {
	return []DetectionRule{
		{
			ID:           "pe.meta",
			DetectorName: "pe-metadata",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindPE},
			Enabled:      true,
		},
		{
			ID:           "pe.url",
			DetectorName: "url-pattern",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindPE},
			Enabled:      true,
		},
		{
			ID:           "pe.ioc",
			DetectorName: "ioc",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindPE},
			Enabled:      true,
		},
		{
			ID:           "pe.keyvalue",
			DetectorName: "key-value",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindPE},
			Enabled:      true,
		},
		{
			ID:           "pe.scheduled_task",
			DetectorName: "scheduled-task",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindPE},
			Enabled:      true,
		},
		{
			ID:           "pe.service",
			DetectorName: "service-artefact",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindPE},
			Enabled:      true,
		},
	}
}
