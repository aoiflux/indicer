package relation

import (
	"regexp"
	"sort"
	"strings"

	"indicer/lib/microartefact/model"
)

var txIDPattern = regexp.MustCompile(`(?i)\bTX[0-9A-Z_-]{2,}\b`)

type SameIdentifierBuilder struct{}

func (SameIdentifierBuilder) Name() string {
	return "same-identifier"
}

func (SameIdentifierBuilder) Build(_ model.FileRecord, artefacts []model.Artefact, method Method) []model.Relation {
	groups := make(map[string][]model.Artefact)
	for _, artefact := range artefacts {
		value := normalizeValue(artefact.Value)
		if value == "" {
			continue
		}
		if !isIdentifierKind(artefact.Kind) {
			continue
		}
		groups[artefact.Kind+"\x00"+value] = append(groups[artefact.Kind+"\x00"+value], artefact)
	}

	relations := make([]model.Relation, 0, 16)
	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		sort.Slice(members, func(i, j int) bool {
			if members[i].Span.Start == members[j].Span.Start {
				return members[i].Span.End < members[j].Span.End
			}
			return members[i].Span.Start < members[j].Span.Start
		})
		for i := 0; i < len(members)-1; i++ {
			for j := i + 1; j < len(members); j++ {
				relations = append(relations, model.Relation{
					FromKind:      members[i].Kind,
					FromValue:     members[i].Value,
					ToKind:        members[j].Kind,
					ToValue:       members[j].Value,
					RelationType:  method.RelationType,
					Method:        method.ID,
					Deterministic: method.Deterministic,
					Confidence:    method.Confidence,
					Evidence: []model.EvidenceField{
						{FieldName: "artefact.value", Parser: members[i].Detector, RawValue: members[i].Value},
						{FieldName: "artefact.value", Parser: members[j].Detector, RawValue: members[j].Value},
					},
				})
			}
		}
	}
	return relations
}

type TransactionPropagationBuilder struct{}

func (TransactionPropagationBuilder) Name() string {
	return "transaction-propagation"
}

func (TransactionPropagationBuilder) Build(_ model.FileRecord, artefacts []model.Artefact, method Method) []model.Relation {
	byTxID := make(map[string][]model.Artefact)
	for _, artefact := range artefacts {
		txid := extractTxID(artefact)
		if txid == "" {
			continue
		}
		byTxID[txid] = append(byTxID[txid], artefact)
	}

	relations := make([]model.Relation, 0, 16)
	for txid, members := range byTxID {
		if len(members) < 2 {
			continue
		}
		for i := 0; i < len(members)-1; i++ {
			for j := i + 1; j < len(members); j++ {
				relations = append(relations, model.Relation{
					FromKind:      members[i].Kind,
					FromValue:     members[i].Value,
					ToKind:        members[j].Kind,
					ToValue:       members[j].Value,
					RelationType:  method.RelationType,
					Method:        method.ID,
					Deterministic: method.Deterministic,
					Confidence:    method.Confidence,
					Evidence: []model.EvidenceField{
						{FieldName: "transaction_id", Parser: members[i].Detector, RawValue: txid},
						{FieldName: "transaction_id", Parser: members[j].Detector, RawValue: txid},
					},
				})
			}
		}
	}
	return relations
}

func DefaultBuilders() []Builder {
	return []Builder{
		SameIdentifierBuilder{},
		TransactionPropagationBuilder{},
	}
}

func DefaultMethods() []Method {
	return []Method{
		{ID: "hash_match", BuilderName: "same-identifier", RelationType: "same_identifier", Deterministic: true, Enabled: true, Confidence: 1.0},
		{ID: "transaction_id_propagation", BuilderName: "transaction-propagation", RelationType: "references_transaction", Deterministic: true, Enabled: true, Confidence: 1.0},
	}
}

func isIdentifierKind(kind string) bool {
	switch strings.ToLower(kind) {
	case "ioc_hash", "ioc_ip", "ioc_email", "ioc_domain", "url", "txid", "serial", "message_id":
		return true
	default:
		return false
	}
}

func normalizeValue(value string) string {
	return strings.TrimSpace(strings.ToLower(value))
}

func extractTxID(artefact model.Artefact) string {
	if strings.EqualFold(artefact.Kind, "txid") {
		return strings.TrimSpace(artefact.Value)
	}
	if strings.EqualFold(artefact.Kind, "key_value") {
		if key, ok := artefact.Attributes["key"]; ok {
			lowerKey := strings.ToLower(strings.TrimSpace(key))
			switch lowerKey {
			case "txid", "transaction_id", "transactionid", "txn", "tx":
				if value, ok := artefact.Attributes["value"]; ok {
					return strings.TrimSpace(value)
				}
			}
		}
	}
	match := txIDPattern.FindString(artefact.Value)
	if match == "" {
		return ""
	}
	return strings.TrimSpace(match)
}
