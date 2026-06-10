package near

import "testing"

func TestParseTrailingRelationIndex(t *testing.T) {
	key := []byte("R|||:abc|||8650752")
	idx, err := parseTrailingRelationIndex(key)
	if err != nil {
		t.Fatalf("parseTrailingRelationIndex returned error: %v", err)
	}
	if idx != 8650752 {
		t.Fatalf("expected 8650752, got %d", idx)
	}
}

func TestParseTrailingRelationIndexHandlesPipeBeforeIndex(t *testing.T) {
	key := []byte("R|||:abc||||8650752")
	idx, err := parseTrailingRelationIndex(key)
	if err != nil {
		t.Fatalf("parseTrailingRelationIndex returned error: %v", err)
	}
	if idx != 8650752 {
		t.Fatalf("expected 8650752, got %d", idx)
	}
}

func TestParseTrailingRelationIndexFailsWithoutIndex(t *testing.T) {
	key := []byte("R|||:abc|||")
	_, err := parseTrailingRelationIndex(key)
	if err == nil {
		t.Fatal("expected error for key without trailing index")
	}
}
