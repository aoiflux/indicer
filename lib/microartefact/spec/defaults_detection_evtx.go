package spec

import parsingpkg "indicer/lib/microartefact/parsing"

func defaultEVTXDetectionRules() []DetectionRule {
	return []DetectionRule{
		{
			ID:           "evtx.native",
			DetectorName: "evtx-native",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindEVTX},
			Enabled:      true,
		},
		{
			ID:           "evtx.windows_event",
			DetectorName: "windows-event-record",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindEVTX},
			Enabled:      true,
		},
	}
}
