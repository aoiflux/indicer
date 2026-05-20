package microartefact

import detectorpkg "indicer/lib/microartefact/detectors"

type Detector = detectorpkg.Detector
type URLDetector = detectorpkg.URLDetector
type KeyValueDetector = detectorpkg.KeyValueDetector

func DefaultDetectors() []Detector {
	return detectorpkg.Default()
}
