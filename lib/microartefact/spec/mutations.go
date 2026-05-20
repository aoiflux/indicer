package spec

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
