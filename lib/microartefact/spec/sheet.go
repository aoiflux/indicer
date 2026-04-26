package spec

import (
	detectionpkg "indicer/lib/microartefact/detection"
	parsingpkg "indicer/lib/microartefact/parsing"
	relationpkg "indicer/lib/microartefact/relation"
)

type DetectionRule struct {
	ID           string
	DetectorName string
	AppliesTo    []parsingpkg.Kind
	Enabled      bool
}

type RelationRule struct {
	ID            string
	BuilderName   string
	RelationType  string
	Deterministic bool
	Enabled       bool
	Confidence    float32
}

type Sheet struct {
	Title          string
	Version        string
	DetectionRules []DetectionRule
	RelationRules  []RelationRule
}

func Default() Sheet {
	return Sheet{
		Title:   "SYNTHRA Micro-Artefact Rulesheet",
		Version: "0.1",
		DetectionRules: []DetectionRule{
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
		},
		RelationRules: []RelationRule{
			{ID: "hash_match", BuilderName: "same-identifier", RelationType: "same_identifier", Deterministic: true, Enabled: true, Confidence: 1.0},
			{ID: "transaction_id_propagation", BuilderName: "transaction-propagation", RelationType: "references_transaction", Deterministic: true, Enabled: true, Confidence: 1.0},
		},
	}
}

func (s Sheet) DetectionMethods() []detectionpkg.Method {
	methods := make([]detectionpkg.Method, 0, len(s.DetectionRules))
	for _, rule := range s.DetectionRules {
		methods = append(methods, detectionpkg.Method{
			ID:           rule.ID,
			DetectorName: rule.DetectorName,
			AppliesTo:    append([]parsingpkg.Kind(nil), rule.AppliesTo...),
			Enabled:      rule.Enabled,
		})
	}
	return methods
}

func (s Sheet) RelationMethods() []relationpkg.Method {
	methods := make([]relationpkg.Method, 0, len(s.RelationRules))
	for _, rule := range s.RelationRules {
		methods = append(methods, relationpkg.Method{
			ID:            rule.ID,
			BuilderName:   rule.BuilderName,
			RelationType:  rule.RelationType,
			Deterministic: rule.Deterministic,
			Enabled:       rule.Enabled,
			Confidence:    rule.Confidence,
		})
	}
	return methods
}

func (s *Sheet) AddDetectionRule(rule DetectionRule) {
	s.DetectionRules = append(s.DetectionRules, rule)
}

func (s *Sheet) RemoveDetectionRule(id string) {
	filtered := s.DetectionRules[:0]
	for _, rule := range s.DetectionRules {
		if rule.ID == id {
			continue
		}
		filtered = append(filtered, rule)
	}
	s.DetectionRules = filtered
}

func (s *Sheet) AddRelationRule(rule RelationRule) {
	s.RelationRules = append(s.RelationRules, rule)
}

func (s *Sheet) RemoveRelationRule(id string) {
	filtered := s.RelationRules[:0]
	for _, rule := range s.RelationRules {
		if rule.ID == id {
			continue
		}
		filtered = append(filtered, rule)
	}
	s.RelationRules = filtered
}
