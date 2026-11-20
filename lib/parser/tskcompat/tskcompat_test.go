package tskcompat

import (
	"bytes"
	"encoding/json"
	"testing"

	"indicer/lib/parser/core"
	"indicer/lib/parser/image"
	"time"
)

func TestRenderValidJSON(t *testing.T) {
	t.Parallel()
	img := image.NewRaw(bytes.NewReader(make([]byte, 1<<20)), 1<<20)
	defer img.Close()

	out, err := Render(img, "/evidence/disk.dd")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var res resultJSON
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if res.Image != "/evidence/disk.dd" {
		t.Fatalf("image = %q, want /evidence/disk.dd", res.Image)
	}
}

func TestToFile(t *testing.T) {
	t.Parallel()
	rec := core.FileRecord{
		Path:      "/dir/a.txt",
		Kind:      core.KindFile,
		Size:      100,
		IsDeleted: true,
		Fragments: []core.AbsRange{
			{Start: 2048, End: 2559},          // real run
			{Start: 0, End: 99, Sparse: true}, // sparse hole — dropped in libtusk shape
			{Start: 4096, End: 4195},          // real run
		},
	}
	f := toFile(rec)

	if f.Filename != "/dir/a.txt" || f.Type != "file" || !f.IsDeleted || f.Size != 100 {
		t.Fatalf("bad file mapping: %+v", f)
	}
	if len(f.Fragments) != 2 {
		t.Fatalf("fragments = %d, want 2 (sparse dropped)", len(f.Fragments))
	}
	if f.Fragments[0].StartOffset != 2048 || f.Fragments[0].EndOffset != 2559 {
		t.Fatalf("fragment[0] = %+v, want {2048,2559}", f.Fragments[0])
	}
	if f.Fragments[1].StartOffset != 4096 || f.Fragments[1].EndOffset != 4195 {
		t.Fatalf("fragment[1] = %+v, want {4096,4195}", f.Fragments[1])
	}
}

// TestToFile_CarriesMACBAndPreservesAbsentTimes covers the boundary where
// timestamps used to be lost. Every filesystem driver populates core.MACB and
// scan.go copies it into core.FileRecord, but this shim rendered the libtusk
// JSON shape, which had no timestamp fields — so the times were parsed and
// discarded on the way to storage.
func TestToFile_CarriesMACBAndPreservesAbsentTimes(t *testing.T) {
	t.Parallel()
	modified := time.Date(2026, 8, 13, 9, 4, 11, 0, time.UTC)
	accessed := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)

	// exFAT's shape: modification and access times, no change time, and here no
	// birth time either.
	got := toFile(core.FileRecord{
		Path: "/Windows/System32/svchost.exe",
		Kind: core.KindFile,
		Size: 55320,
		MACB: core.MACB{Modified: &modified, Accessed: &accessed},
	})

	if got.Modified == nil || *got.Modified != modified.UnixNano() {
		t.Errorf("Modified = %v, want %d", got.Modified, modified.UnixNano())
	}
	if got.Accessed == nil || *got.Accessed != accessed.UnixNano() {
		t.Errorf("Accessed = %v, want %d", got.Accessed, accessed.UnixNano())
	}
	if got.Changed != nil {
		t.Errorf("Changed = %d, want nil", *got.Changed)
	}
	if got.Born != nil {
		t.Errorf("Born = %d, want nil", *got.Born)
	}
}

// A zero time is the "not carried" sentinel fsutil.TimePtr already uses; it
// must not be rendered as an epoch timestamp.
func TestToFile_ZeroTimeIsAbsentNotEpoch(t *testing.T) {
	t.Parallel()
	zero := time.Time{}
	got := toFile(core.FileRecord{
		Path: "/notes.txt",
		Kind: core.KindFile,
		MACB: core.MACB{Modified: &zero},
	})
	if got.Modified != nil {
		t.Errorf("Modified = %d, want nil; a zero time asserts the epoch, which no "+
			"filesystem reported", *got.Modified)
	}
}

// A file with no timestamps must serialise to exactly the legacy shape, so the
// extension stays invisible to anything reading the original contract.
func TestToFile_NoTimesOmitsTheFieldsEntirely(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(toFile(core.FileRecord{
		Path: "/notes.txt", Kind: core.KindFile, Size: 10,
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"mtime", "atime", "ctime", "btime"} {
		if bytes.Contains(encoded, []byte(`"`+key+`"`)) {
			t.Errorf("a file with no timestamps emitted %q: %s", key, encoded)
		}
	}
}
