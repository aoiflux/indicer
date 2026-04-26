package detection

import (
	"indicer/lib/microartefact/model"
	parsingpkg "indicer/lib/microartefact/parsing"

	detectorpkg "indicer/lib/microartefact/detectors"
)

type Method struct {
	ID           string
	DetectorName string
	AppliesTo    []parsingpkg.Kind
	Enabled      bool
}

type Engine struct {
	methods       []Method
	detectorsByID map[string]detectorpkg.Detector
}

func NewEngine(detectors []detectorpkg.Detector, methods []Method) *Engine {
	if len(detectors) == 0 {
		detectors = detectorpkg.Default()
	}
	if len(methods) == 0 {
		methods = DefaultMethods()
	}

	lookup := make(map[string]detectorpkg.Detector, len(detectors))
	for _, detector := range detectors {
		lookup[detector.Name()] = detector
	}

	return &Engine{methods: methods, detectorsByID: lookup}
}

func (e *Engine) Detect(file model.FileRecord, parsed parsingpkg.Result) ([]model.Artefact, error) {
	allowed := make(map[string]struct{})
	for _, method := range e.methods {
		if !method.Enabled {
			continue
		}
		if appliesToKind(method.AppliesTo, parsed.Kind) {
			allowed[method.DetectorName] = struct{}{}
		}
	}

	artefacts := make([]model.Artefact, 0, 128)
	for detectorName := range allowed {
		detector, ok := e.detectorsByID[detectorName]
		if !ok {
			continue
		}
		detected, err := detector.Detect(file, parsed.Content)
		if err != nil {
			return nil, err
		}
		artefacts = append(artefacts, detected...)
	}
	return artefacts, nil
}

func appliesToKind(kinds []parsingpkg.Kind, kind parsingpkg.Kind) bool {
	for _, next := range kinds {
		if next == kind {
			return true
		}
	}
	return false
}

func DefaultMethods() []Method {
	return []Method{
		{ID: "text.url", DetectorName: "url-pattern", AppliesTo: []parsingpkg.Kind{parsingpkg.KindText}, Enabled: true},
		{ID: "text.ioc", DetectorName: "ioc", AppliesTo: []parsingpkg.Kind{parsingpkg.KindText}, Enabled: true},
		{ID: "text.keyvalue", DetectorName: "key-value", AppliesTo: []parsingpkg.Kind{parsingpkg.KindText}, Enabled: true},
		{ID: "text.logline", DetectorName: "log-line", AppliesTo: []parsingpkg.Kind{parsingpkg.KindText}, Enabled: true},
		{ID: "text.windows_event", DetectorName: "windows-event-record", AppliesTo: []parsingpkg.Kind{parsingpkg.KindText}, Enabled: true},
		{ID: "text.scheduled_task", DetectorName: "scheduled-task", AppliesTo: []parsingpkg.Kind{parsingpkg.KindText}, Enabled: true},
		{ID: "text.service", DetectorName: "service-artefact", AppliesTo: []parsingpkg.Kind{parsingpkg.KindText}, Enabled: true},
		{ID: "text.browser", DetectorName: "browser-artefact", AppliesTo: []parsingpkg.Kind{parsingpkg.KindText}, Enabled: true},
		{ID: "pdf.url", DetectorName: "url-pattern", AppliesTo: []parsingpkg.Kind{parsingpkg.KindPDF}, Enabled: true},
		{ID: "pdf.ioc", DetectorName: "ioc", AppliesTo: []parsingpkg.Kind{parsingpkg.KindPDF}, Enabled: true},
		{ID: "pdf.keyvalue", DetectorName: "key-value", AppliesTo: []parsingpkg.Kind{parsingpkg.KindPDF}, Enabled: true},
		{ID: "pdf.logline", DetectorName: "log-line", AppliesTo: []parsingpkg.Kind{parsingpkg.KindPDF}, Enabled: true},
		{ID: "pe.meta", DetectorName: "pe-metadata", AppliesTo: []parsingpkg.Kind{parsingpkg.KindPE}, Enabled: true},
		{ID: "pe.url", DetectorName: "url-pattern", AppliesTo: []parsingpkg.Kind{parsingpkg.KindPE}, Enabled: true},
		{ID: "pe.ioc", DetectorName: "ioc", AppliesTo: []parsingpkg.Kind{parsingpkg.KindPE}, Enabled: true},
		{ID: "pe.keyvalue", DetectorName: "key-value", AppliesTo: []parsingpkg.Kind{parsingpkg.KindPE}, Enabled: true},
		{ID: "pe.scheduled_task", DetectorName: "scheduled-task", AppliesTo: []parsingpkg.Kind{parsingpkg.KindPE}, Enabled: true},
		{ID: "pe.service", DetectorName: "service-artefact", AppliesTo: []parsingpkg.Kind{parsingpkg.KindPE}, Enabled: true},
		{ID: "elf.url", DetectorName: "url-pattern", AppliesTo: []parsingpkg.Kind{parsingpkg.KindELF}, Enabled: true},
		{ID: "elf.ioc", DetectorName: "ioc", AppliesTo: []parsingpkg.Kind{parsingpkg.KindELF}, Enabled: true},
		{ID: "elf.keyvalue", DetectorName: "key-value", AppliesTo: []parsingpkg.Kind{parsingpkg.KindELF}, Enabled: true},
		{ID: "elf.logline", DetectorName: "log-line", AppliesTo: []parsingpkg.Kind{parsingpkg.KindELF}, Enabled: true},
		{ID: "elf.service", DetectorName: "service-artefact", AppliesTo: []parsingpkg.Kind{parsingpkg.KindELF}, Enabled: true},
		{ID: "evtx.native", DetectorName: "evtx-native", AppliesTo: []parsingpkg.Kind{parsingpkg.KindEVTX}, Enabled: true},
		{ID: "evtx.windows_event", DetectorName: "windows-event-record", AppliesTo: []parsingpkg.Kind{parsingpkg.KindEVTX}, Enabled: true},
	}
}
