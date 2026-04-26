package relation

import (
	"testing"

	"indicer/lib/microartefact/model"
)

func TestEngineRelateAppliesEnabledMethods(t *testing.T) {
	engine := NewEngine(DefaultBuilders(), []Method{
		{ID: "hash_match", BuilderName: "same-identifier", RelationType: "same_identifier", Deterministic: true, Enabled: true, Confidence: 1},
		{ID: "tx_match", BuilderName: "transaction-propagation", RelationType: "references_transaction", Deterministic: true, Enabled: true, Confidence: 1},
	})

	artefacts := []model.Artefact{
		{Kind: "ioc_hash", Detector: "ioc", Value: "abc"},
		{Kind: "ioc_hash", Detector: "ioc", Value: "abc"},
		{Kind: "key_value", Detector: "key-value", Value: "transaction_id=TX123", Attributes: map[string]string{"key": "transaction_id", "value": "TX123"}},
		{Kind: "key_value", Detector: "key-value", Value: "transaction_id=TX123", Attributes: map[string]string{"key": "transaction_id", "value": "TX123"}},
	}

	relations := engine.Relate(model.FileRecord{Name: "f.txt"}, artefacts)
	if len(relations) == 0 {
		t.Fatal("expected relations, got none")
	}

	counts := map[string]int{}
	for _, relation := range relations {
		counts[relation.RelationType]++
	}
	if counts["same_identifier"] == 0 {
		t.Fatalf("expected same_identifier relations, got %#v", relations)
	}
	if counts["references_transaction"] == 0 {
		t.Fatalf("expected references_transaction relations, got %#v", relations)
	}
}
