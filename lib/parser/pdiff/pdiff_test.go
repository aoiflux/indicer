package pdiff

import "testing"

func TestCompareIdentical(t *testing.T) {
	j := `{"partitions":[{"filesystem":{"type":"ntfs"},"files":[
		{"filename":"/a.txt","type":"file","size":100,"fragments":[{"start_offset":2048,"end_offset":2147}]},
		{"filename":"/b.txt","type":"file","size":50,"fragments":[{"start_offset":4096,"end_offset":4145}]}
	]}]}`
	rep, err := Compare(j, j)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Match() {
		t.Fatalf("identical inputs should match; got %+v", rep)
	}
	if rep.TuskFiles != 2 || rep.GoFiles != 2 {
		t.Fatalf("file counts = %d/%d, want 2/2", rep.TuskFiles, rep.GoFiles)
	}
}

func TestCompareDifferences(t *testing.T) {
	tusk := `{"partitions":[{"filesystem":{"type":"ntfs"},"files":[
		{"filename":"/a.txt","size":100,"fragments":[{"start_offset":2048,"end_offset":2147}]},
		{"filename":"/only-tusk.txt","size":10,"fragments":[]}
	]}]}`
	goj := `{"partitions":[{"filesystem":{"type":"ntfs"},"files":[
		{"filename":"/a.txt","size":200,"fragments":[{"start_offset":2048,"end_offset":2147},{"start_offset":8192,"end_offset":8291}]},
		{"filename":"/only-go.txt","size":5,"fragments":[]}
	]}]}`
	rep, err := Compare(tusk, goj)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Match() {
		t.Fatal("expected differences")
	}
	if len(rep.OnlyInTusk) != 1 || rep.OnlyInTusk[0] != "/only-tusk.txt" {
		t.Fatalf("OnlyInTusk = %v", rep.OnlyInTusk)
	}
	if len(rep.OnlyInGo) != 1 || rep.OnlyInGo[0] != "/only-go.txt" {
		t.Fatalf("OnlyInGo = %v", rep.OnlyInGo)
	}
	if len(rep.SizeMismatch) != 1 || rep.SizeMismatch[0].Tusk != 100 || rep.SizeMismatch[0].Go != 200 {
		t.Fatalf("SizeMismatch = %+v", rep.SizeMismatch)
	}
	if len(rep.FragMismatch) != 1 || rep.FragMismatch[0].TuskRuns != 1 || rep.FragMismatch[0].GoRuns != 2 {
		t.Fatalf("FragMismatch = %+v", rep.FragMismatch)
	}
}

func TestCompareFlatLayout(t *testing.T) {
	// Flat (single-filesystem) layout, no partitions[].
	j := `{"filesystem":{"type":"exfat"},"files":[{"filename":"/x","size":1,"fragments":[]}]}`
	rep, err := Compare(j, j)
	if err != nil {
		t.Fatal(err)
	}
	if rep.TuskFiles != 1 || !rep.Match() {
		t.Fatalf("flat layout: files=%d match=%v", rep.TuskFiles, rep.Match())
	}
}
