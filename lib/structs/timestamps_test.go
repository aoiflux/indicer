package structs

import (
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

func ptr(n int64) *int64 { return &n }

// legacyIndexedFile is the shape IndexedFile had before timestamps existed. It
// is declared here rather than derived, so the compatibility assertions below
// are checked against a fixed target that cannot drift with the real type.
type legacyIndexedFile struct {
	Name         string `msgpack:"name"`
	Size         int64  `msgpack:"size"`
	Start        int64  `msgpack:"start"`
	IndexedType  string `msgpack:"indexed_type"`
	IsDeleted    bool   `msgpack:"is_deleted"`
	IsFragmented bool   `msgpack:"is_fragmented"`
	FileHash     string `msgpack:"file_hash"`
}

// TestTimestamps_AddingThemIsByteIdentical is the guarantee that made this a
// safe change to an on-disk schema holding real cases: a record with no
// timestamps encodes to exactly the bytes it encoded to before the field
// existed. No migration, and no rewrite of stored evidence.
func TestTimestamps_AddingThemIsByteIdentical(t *testing.T) {
	t.Parallel()
	legacy := legacyIndexedFile{
		Name: "/Windows/System32/svchost.exe", Size: 55320, Start: 4096,
		IndexedType: "pe", IsDeleted: false, IsFragmented: true, FileHash: "aGFzaA==",
	}
	current := NewIndexedFile(legacy.Name, legacy.Start, legacy.Size,
		legacy.IndexedType, legacy.IsDeleted, legacy.FileHash)
	current.IsFragmented = legacy.IsFragmented

	legacyBytes, err := msgpack.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	currentBytes, err := msgpack.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}

	if string(legacyBytes) != string(currentBytes) {
		t.Errorf("a record without timestamps no longer encodes identically:\n"+
			" legacy %d bytes\ncurrent %d bytes\n"+
			"the timestamp field must stay omitempty or every stored record churns",
			len(legacyBytes), len(currentBytes))
	}
}

// A record written before timestamps existed must still decode, with every
// time reported as absent rather than as the epoch.
func TestTimestamps_LegacyRecordDecodesWithNoTimes(t *testing.T) {
	t.Parallel()
	legacyBytes, err := msgpack.Marshal(legacyIndexedFile{
		Name: "notes.txt", Size: 1041, IndexedType: "txt", IsDeleted: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	var decoded IndexedFile
	if err := msgpack.Unmarshal(legacyBytes, &decoded); err != nil {
		t.Fatalf("a pre-timestamp record failed to decode: %v", err)
	}
	if decoded.Name != "notes.txt" || decoded.Size != 1041 || !decoded.IsDeleted {
		t.Errorf("existing fields did not survive: %+v", decoded)
	}
	if decoded.Times != nil {
		t.Errorf("Times = %+v, want nil; a record that never carried timestamps "+
			"must not report any", decoded.Times)
	}
	if decoded.Times.Any() {
		t.Error("Any() on a nil Times reported true")
	}
}

// The nil-versus-zero distinction is the whole point: ext records no birth
// time and exFAT no change time, and reporting the epoch for those would
// assert a timestamp no evidence supports.
func TestTimestamps_PartialTimesRoundTripWithNilsIntact(t *testing.T) {
	t.Parallel()
	const mtime, atime = int64(1755075851000000000), int64(1755075999000000000)

	original := NewIndexedFile("History", 0, 262144, "sqlite", false, "aGlzdG9yeQ==")
	original.Times = &Timestamps{Modified: ptr(mtime), Accessed: ptr(atime)}

	encoded, err := msgpack.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded IndexedFile
	if err := msgpack.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}

	if decoded.Times == nil {
		t.Fatal("Times went missing across the round trip")
	}
	if decoded.Times.Modified == nil || *decoded.Times.Modified != mtime {
		t.Errorf("Modified = %v, want %d", decoded.Times.Modified, mtime)
	}
	if decoded.Times.Accessed == nil || *decoded.Times.Accessed != atime {
		t.Errorf("Accessed = %v, want %d", decoded.Times.Accessed, atime)
	}
	if decoded.Times.Changed != nil {
		t.Errorf("Changed = %d, want nil — exFAT and FAT carry no change time, and "+
			"a zero value would assert one", *decoded.Times.Changed)
	}
	if decoded.Times.Born != nil {
		t.Errorf("Born = %d, want nil — ext carries no birth time", *decoded.Times.Born)
	}
}

// A newer record must stay readable by code that predates the field, so a
// mixed-version deployment cannot corrupt a case.
func TestTimestamps_NewRecordDecodesIntoTheOldShape(t *testing.T) {
	t.Parallel()
	current := NewIndexedFile("svchost.exe", 4096, 55320, "pe", false, "aGFzaA==")
	current.Times = &Timestamps{Modified: ptr(1755075851000000000)}

	encoded, err := msgpack.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	var old legacyIndexedFile
	if err := msgpack.Unmarshal(encoded, &old); err != nil {
		t.Fatalf("a timestamped record broke the old decoder: %v", err)
	}
	if old.Name != "svchost.exe" || old.Size != 55320 || old.FileHash != "aGFzaA==" {
		t.Errorf("fields did not survive the old decoder: %+v", old)
	}
}

func TestTimestamps_AnyReportsPresence(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		times *Timestamps
		want  bool
	}{
		"nil":            {nil, false},
		"empty":          {&Timestamps{}, false},
		"modified only":  {&Timestamps{Modified: ptr(1)}, true},
		"born only":      {&Timestamps{Born: ptr(1)}, true},
		"zero but a set": {&Timestamps{Changed: ptr(0)}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.times.Any(); got != tc.want {
				t.Errorf("Any() = %v, want %v", got, tc.want)
			}
		})
	}
}
