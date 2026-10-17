package detection

import (
	"testing"

	"indicer/internal/safe"
	detectorpkg "indicer/lib/microartefact/detectors"
	"indicer/lib/microartefact/model"
	parsingpkg "indicer/lib/microartefact/parsing"
)

type panicDetector struct{}

func (panicDetector) Name() string { return "boom" }
func (panicDetector) Detect(model.FileRecord, []byte) ([]model.Artefact, error) {
	var b []byte
	_ = b[3] // a detector misreading hostile input
	return nil, nil
}

// TestDetectContainsPanics: a detector that panics on evidence fails that
// file with an error instead of ending the process (PAR-1).
func TestDetectContainsPanics(t *testing.T) {
	e := NewEngine([]detectorpkg.Detector{panicDetector{}}, []Method{{ID: "m", DetectorName: "boom", AppliesTo: []parsingpkg.Kind{parsingpkg.KindText}, Enabled: true}})
	_, err := e.Detect(model.FileRecord{}, parsingpkg.Result{Kind: parsingpkg.KindText, Content: []byte("x")})
	if !safe.IsPanic(err) {
		t.Fatalf("err %v, want a recovered panic", err)
	}
}
