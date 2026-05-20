package spec

import (
	detectionpkg "indicer/lib/microartefact/detection"
	parsingpkg "indicer/lib/microartefact/parsing"
	relationpkg "indicer/lib/microartefact/relation"
)

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
