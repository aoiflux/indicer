package spec

const (
	defaultSheetTitle   = "SYNTHRA Micro-Artefact Rulesheet"
	defaultSheetVersion = "0.1"
)

func Default() Sheet {
	return Sheet{
		Title:          defaultSheetTitle,
		Version:        defaultSheetVersion,
		DetectionRules: defaultDetectionRules(),
		RelationRules:  defaultRelationRules(),
	}
}

func defaultDetectionRules() []DetectionRule {
	rules := make([]DetectionRule, 0,
		len(defaultTextDetectionRules())+
			len(defaultPDFDetectionRules())+
			len(defaultPEDetectionRules())+
			len(defaultELFDetectionRules())+
			len(defaultEVTXDetectionRules()),
	)

	rules = append(rules, defaultTextDetectionRules()...)
	rules = append(rules, defaultPDFDetectionRules()...)
	rules = append(rules, defaultPEDetectionRules()...)
	rules = append(rules, defaultELFDetectionRules()...)
	rules = append(rules, defaultEVTXDetectionRules()...)

	return rules
}

func defaultRelationRules() []RelationRule {
	return []RelationRule{
		{
			ID:            "hash_match",
			BuilderName:   "same-identifier",
			RelationType:  "same_identifier",
			Deterministic: true,
			Enabled:       true,
			Confidence:    1.0,
		},
		{
			ID:            "transaction_id_propagation",
			BuilderName:   "transaction-propagation",
			RelationType:  "references_transaction",
			Deterministic: true,
			Enabled:       true,
			Confidence:    1.0,
		},
	}
}
