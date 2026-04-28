package detectors

func Default() []Detector {
	return []Detector{
		URLDetector{},
		IOCDetector{},
		KeyValueDetector{},
		PEMetadataDetector{},
		RegistryHiveDetector{},
		EVTXDetector{},
		LogLineDetector{},
		WindowsEventRecordDetector{},
		ScheduledTaskDetector{},
		ServiceArtefactDetector{},
		BrowserArtefactDetector{},
	}
}
