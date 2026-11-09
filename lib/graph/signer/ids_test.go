package signer

import (
	"strings"
	"testing"
)

// TestIDVectors pins the derivations, computed apart from this code: an ID
// that changed would orphan every signature already written.
func TestIDVectors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		want uint64
	}{
		{"database", 0xddfc1bec65f6b4fc},
		{"Jane Doe ", 0x64e73b31ed2eb40e},
		{"  jane doe", 0x64e73b31ed2eb40e},
		{"examiner-01", 0x8f56dd6450c89522},
	} {
		if got := ActorID(tc.name); got != tc.want {
			t.Errorf("ActorID(%q) = %016x, want %016x", tc.name, got, tc.want)
		}
	}
	pub := make([]byte, 32)
	for i := range pub {
		pub[i] = byte(i)
	}
	if got, want := KeyID(pub), uint64(0x16f0a60e505b4f09); got != want {
		t.Errorf("KeyID = %016x, want %016x", got, want)
	}
}

func TestFormatParseID(t *testing.T) {
	t.Parallel()
	for _, id := range []uint64{1, 0xddfc1bec65f6b4fc, ^uint64(0)} {
		got, err := ParseID(FormatID(id))
		if err != nil || got != id {
			t.Errorf("ParseID(FormatID(%x)) = %x, %v", id, got, err)
		}
	}
	for _, s := range []string{"", "0000000000000000", "DDFC1BEC65F6B4FC", "ddfc1bec65f6b4f", "+ddfc1bec65f6b4f", "ddfc1bec65f6b4fcc", "zzfc1bec65f6b4fc"} {
		if _, err := ParseID(s); err == nil {
			t.Errorf("ParseID(%q) accepted", s)
		}
	}
}

func TestNormalizeName(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		" Jane Doe ":       "jane doe",
		"DATABASE":         "database",
		"examiner@lab.org": "examiner@lab.org",
		"Ölçer":            "ölçer",
	} {
		if got, err := NormalizeName(in); err != nil || got != want {
			t.Errorf("NormalizeName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", "tab\there", "nul\x00", "bad\xff", strings.Repeat("a", maxNameRunes+1)} {
		if got, err := NormalizeName(in); err == nil {
			t.Errorf("NormalizeName(%q) = %q, accepted", in, got)
		}
	}
}

// TestRegistration: a name already in the table is registered; a different
// name with the same ID is a collision.
func TestRegistration(t *testing.T) {
	t.Parallel()
	actors := []Actor{{Name: "database", ID: ActorID("database")}, {Name: "jane", ID: 42}}
	if ok, err := registration(actors, "database", ActorID("database")); !ok || err != nil {
		t.Fatalf("registered name: %v, %v", ok, err)
	}
	if ok, err := registration(actors, "john", ActorID("john")); ok || err != nil {
		t.Fatalf("new name: %v, %v", ok, err)
	}
	if _, err := registration(actors, "mallory", 42); err == nil || !strings.Contains(err.Error(), `same ID (000000000000002a) as actor "jane"`) {
		t.Fatalf("collision: %v", err)
	}
}
