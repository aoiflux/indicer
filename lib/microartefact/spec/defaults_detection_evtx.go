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

// defaultRegistryDetectionRules run the registry detector on hives, which
// no rule named before, so it never ran (GRA-13).
func defaultRegistryDetectionRules() []DetectionRule {
	return []DetectionRule{
		{
			ID:           "registry.hive",
			DetectorName: "registry-hive",
			AppliesTo:    []parsingpkg.Kind{parsingpkg.KindRegistry},
			Enabled:      true,
		},
	}
}
