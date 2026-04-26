package spec

import (
	"testing"

	parsingpkg "indicer/lib/microartefact/parsing"
)

func TestSheetAddRemoveDetectionRule(t *testing.T) {
	sheet := Default()
	base := len(sheet.DetectionRules)

	rule := DetectionRule{
		ID:           "custom.email.message_id",
		DetectorName: "email-header",
		AppliesTo:    []parsingpkg.Kind{parsingpkg.KindText},
		Enabled:      true,
	}
	sheet.AddDetectionRule(rule)

	if len(sheet.DetectionRules) != base+1 {
		t.Fatalf("expected %d detection rules, got %d", base+1, len(sheet.DetectionRules))
	}

	sheet.RemoveDetectionRule(rule.ID)
	if len(sheet.DetectionRules) != base {
		t.Fatalf("expected %d detection rules after remove, got %d", base, len(sheet.DetectionRules))
	}
}

func TestSheetAddRemoveRelationRule(t *testing.T) {
	sheet := Default()
	base := len(sheet.RelationRules)

	rule := RelationRule{
		ID:            "custom.email.attachment",
		BuilderName:   "email-attachment-linker",
		RelationType:  "attached_to",
		Deterministic: true,
		Enabled:       true,
		Confidence:    1,
	}
	sheet.AddRelationRule(rule)

	if len(sheet.RelationRules) != base+1 {
		t.Fatalf("expected %d relation rules, got %d", base+1, len(sheet.RelationRules))
	}

	sheet.RemoveRelationRule(rule.ID)
	if len(sheet.RelationRules) != base {
		t.Fatalf("expected %d relation rules after remove, got %d", base, len(sheet.RelationRules))
	}
}
