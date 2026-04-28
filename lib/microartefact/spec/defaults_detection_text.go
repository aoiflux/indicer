package spec

import parsingpkg "indicer/lib/microartefact/parsing"

func defaultTextDetectionRules() []DetectionRule {
	return []DetectionRule{
		{
			ID:           "text.url",
			DetectorName: "url-pattern",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindText},
			Enabled:      true,
		},
		{
			ID:           "text.ioc",
			DetectorName: "ioc",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindText},
			Enabled:      true,
		},
		{
			ID:           "text.keyvalue",
			DetectorName: "key-value",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindText},
			Enabled:      true,
		},
		{
			ID:           "text.logline",
			DetectorName: "log-line",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindText},
			Enabled:      true,
		},
		{
			ID:           "text.windows_event",
			DetectorName: "windows-event-record",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindText},
			Enabled:      true,
		},
		{
			ID:           "text.scheduled_task",
			DetectorName: "scheduled-task",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindText},
			Enabled:      true,
		},
		{
			ID:           "text.service",
			DetectorName: "service-artefact",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindText},
			Enabled:      true,
		},
		{
			ID:           "text.browser",
			DetectorName: "browser-artefact",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindText},
			Enabled:      true,
		},
	}
}
