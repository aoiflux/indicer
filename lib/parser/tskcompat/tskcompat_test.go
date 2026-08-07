package tskcompat

import (
	"bytes"
	"encoding/json"
	"testing"

	"indicer/lib/parser/core"
	"indicer/lib/parser/image"
)

func TestRenderValidJSON(t *testing.T) {
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
