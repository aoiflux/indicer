package spec

func (s *Sheet) AddDetectionRule(rule DetectionRule) {
	s.DetectionRules = append(s.DetectionRules, rule)
}

// RemoveDetectionRule removes a rule into a new slice, so a sheet copied
// from this one (which shares the backing array) keeps its rules. Filtering
// in place rewrote the shared array (GRA-18).
func (s *Sheet) RemoveDetectionRule(id string) {
	filtered := make([]DetectionRule, 0, len(s.DetectionRules))
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

// RemoveRelationRule removes a rule into a new slice; see RemoveDetectionRule.
func (s *Sheet) RemoveRelationRule(id string) {
	filtered := make([]RelationRule, 0, len(s.RelationRules))
	for _, rule := range s.RelationRules {
		if rule.ID == id {
			continue
		}
		filtered = append(filtered, rule)
	}
	s.RelationRules = filtered
}
