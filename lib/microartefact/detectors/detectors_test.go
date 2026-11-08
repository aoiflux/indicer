package detectors

import (
	"testing"

	"indicer/lib/microartefact/model"

	"www.velocidex.com/golang/regparser"
)

func TestRegistryHiveDetectorIgnoresNonHiveData(t *testing.T) {
	t.Parallel()
	detector := RegistryHiveDetector{}
	artefacts, err := detector.Detect(model.FileRecord{Name: "not-hive.bin"}, []byte("not a registry hive"))
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	if len(artefacts) != 0 {
		t.Fatalf("expected no artefacts for non-hive data, got %d", len(artefacts))
	}
}

func TestEVTXDetectorIgnoresNonEVTXData(t *testing.T) {
	t.Parallel()
	detector := EVTXDetector{}
	artefacts, err := detector.Detect(model.FileRecord{Name: "not-evtx.bin"}, []byte("not an evtx file"))
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	if len(artefacts) != 0 {
		t.Fatalf("expected no artefacts for non-evtx data, got %d", len(artefacts))
	}
}

func TestRegistryValueStringFormatsMultiSZ(t *testing.T) {
	t.Parallel()
	formatted := registryValueString(&regparser.ValueData{MultiSz: []string{"alpha", "beta"}})
	if formatted != "alpha;beta" {
		t.Fatalf("unexpected formatted MultiSZ value: %q", formatted)
	}
}
