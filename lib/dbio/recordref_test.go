package dbio

import (
	"bytes"
	"encoding/base64"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/structs"

	"github.com/vmihailenco/msgpack/v5"
)

func TestRecordRefsReadBothForms(t *testing.T) {
	t.Parallel()
	eid := []byte("0123456789abcdef")
	pid := []byte("partition-id-xyz")
	sep := cnst.DataSeperator
	pidB64 := base64.StdEncoding.EncodeToString(pid)

	cases := []struct {
		name     string
		rec      structs.IndexedFile
		wantPath string
		wantPID  []byte
	}{
		{
			name:     "explicit",
			rec:      structs.IndexedFile{EvidenceID: eid, PartitionID: pid, Path: "/a|||b.txt"},
			wantPath: "/a|||b.txt",
			wantPID:  pid,
		},
		{
			name:     "legacy indexed, raw evidence ID",
			rec:      newIndexed(string(eid) + sep + pidB64 + sep + "/dir/a|||b.txt"),
			wantPath: "/dir/a|||b.txt",
			wantPID:  pid,
		},
		{
			// A raw ID may hold the separator itself; it is found at byte 16.
			name:     "legacy indexed, raw evidence ID with separator inside",
			rec:      newIndexed("01|||4567890abcd" + sep + pidB64 + sep + "/x"),
			wantPath: "/x",
			wantPID:  pid,
		},
		{
			name:     "legacy partition",
			rec:      newIndexed(base64.StdEncoding.EncodeToString(eid) + sep + "disk.img_p0"),
			wantPath: "disk.img_p0",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RecordPath(c.rec); got != c.wantPath {
				t.Errorf("path = %q, want %q", got, c.wantPath)
			}
			got, err := RecordEvidenceID(c.rec)
			if err != nil {
				t.Fatal(err)
			}
			want := eid
			if c.name == "legacy indexed, raw evidence ID with separator inside" {
				want = []byte("01|||4567890abcd")
			}
			if !bytes.Equal(got, want) {
				t.Errorf("evidence ID = %q, want %q", got, want)
			}
			if c.wantPID == nil {
				return
			}
			p, err := RecordPartitionID(c.rec)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(p, c.wantPID) {
				t.Errorf("partition ID = %q, want %q", p, c.wantPID)
			}
		})
	}
}

func newIndexed(name string) structs.IndexedFile {
	return structs.NewIndexedFile(name, 0, 1, "", false, "")
}

// legacyIndexed is the shape of an indexed record before W6.
type legacyIndexed struct {
	Name         string `msgpack:"name"`
	Size         int64  `msgpack:"size"`
	Start        int64  `msgpack:"start"`
	IndexedType  string `msgpack:"indexed_type"`
	IsDeleted    bool   `msgpack:"is_deleted"`
	IsFragmented bool   `msgpack:"is_fragmented"`
	FileHash     string `msgpack:"file_hash"`
}

func TestRecordsDecodeLegacyAndNewShapes(t *testing.T) {
	t.Parallel()
	old := legacyIndexed{Name: "n", Size: 5, Start: 512, IndexedType: "text", FileHash: "h"}
	data, err := msgpack.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var rec structs.IndexedFile
	if err := msgpack.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Name != "n" || rec.Size != 5 || rec.Start != 512 || rec.FileHash != "h" || rec.Content != nil || rec.EvidenceID != nil {
		t.Fatalf("legacy record decoded as %+v", rec)
	}

	rec = structs.IndexedFile{
		EvidenceID: []byte{1}, Path: "/p",
		Content: &structs.ContentRef{Kind: structs.ContentExtents, Located: 30, Extents: []structs.Extent{
			{FileOffset: 0, Length: 10, Offset: 4096, Kind: structs.ExtentData},
			{FileOffset: 10, Length: 20, Kind: structs.ExtentSparse},
		}},
	}
	data, err = msgpack.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var back structs.IndexedFile
	if err := msgpack.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Content == nil || len(back.Content.Extents) != 2 || back.Content.Extents[0].Offset != 4096 || back.Content.Extents[1].Kind != structs.ExtentSparse {
		t.Fatalf("round trip gave %+v", back.Content)
	}
	// An old reader of the new record still sees its legacy fields.
	var oldView legacyIndexed
	if err := msgpack.Unmarshal(data, &oldView); err != nil {
		t.Fatalf("legacy shape cannot read a new record: %v", err)
	}
}
