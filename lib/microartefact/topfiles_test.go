package microartefact

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"indicer/lib/enrichment"
)

// TestTopFiles (TUI-5): the bounded view holds the files with the most
// artefacts, most first, at most perFile artefacts of each, under their
// partitions and evidence, with exact totals over every file.
func TestTopFiles(t *testing.T) {
	root := t.TempDir()
	enr, err := enrichment.OpenGrapheneRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	type file struct{ id, ev, part string }
	files := []file{{"a", "ev2", "p1"}, {"b", "ev1", "p1"}, {"c", "ev1", "p2"}, {"none", "ev1", "p2"}}
	for _, f := range files {
		if err := enr.UpsertFile(enrichment.FileRecord{Level: "indexed_file", EvidenceFileID: f.ev, EvidenceFileName: f.ev + ".img",
			PartitionID: f.ev + "/" + f.part, PartitionName: f.part, IndexedFileID: f.id, FileName: f.id + ".log", Path: "/" + f.id + ".log"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := enr.Close(); err != nil {
		t.Fatal(err)
	}

	repo, err := OpenGrapheneRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	counts := map[string]int{}
	total := 0
	for i, f := range files {
		var content strings.Builder
		ips := []int{30, 10, 3, 0}[i]
		for n := range ips {
			fmt.Fprintf(&content, "contact 10.1.%d.%d now\n", i, n)
		}
		if ips == 0 {
			content.WriteString("nothing to see\n")
		}
		rec := FileRecord{Name: f.id + ".log", Path: "/" + f.id + ".log", FileType: "log", EvidenceFileID: f.ev, PartitionID: f.ev + "/" + f.part, IndexedFileID: f.id}
		out, err := NewService(repo).ProcessFile(rec, []byte(content.String()), int64(content.Len()), false)
		if err != nil {
			t.Fatal(err)
		}
		counts[f.id] = out.Artefacts
		total += out.Artefacts
	}
	if !(counts["a"] > counts["b"] && counts["b"] > counts["c"] && counts["c"] > 0 && counts["none"] == 0) {
		t.Fatalf("artefact counts %v: want a > b > c > 0 and none", counts)
	}

	top, err := repo.TopFiles(t.Context(), 2, 5)
	if err != nil {
		t.Fatal(err)
	}
	if top.Files != 3 || top.Artefacts != total || top.Shown != 2 {
		t.Fatalf("totals: files %d, artefacts %d (want %d), shown %d", top.Files, top.Artefacts, total, top.Shown)
	}
	var shown []string
	for _, ev := range top.Tree.EvidenceFiles {
		for _, p := range ev.Partitions {
			for _, f := range p.Files {
				shown = append(shown, fmt.Sprintf("%s/%s:%s:%s", ev.Name, p.Name, f.Name, f.Path))
				if f.ArtefactCount != counts[strings.Trim(f.Path, "/.log")] || len(f.Artefacts) != 5 {
					t.Errorf("%s: count %d, %d artefacts shown; want %d and 5", f.Path, f.ArtefactCount, len(f.Artefacts), counts[strings.Trim(f.Path, "/.log")])
				}
			}
		}
	}
	if got, want := strings.Join(shown, " "), "ev1.img/p1:b.log:/b.log ev2.img/p1:a.log:/a.log"; got != want {
		t.Fatalf("shown %q, want %q", got, want)
	}

	all, err := repo.TopFiles(t.Context(), 0, 0)
	if err != nil || all.Shown != 3 {
		t.Fatalf("no limit: %v, shown %d", err, all.Shown)
	}
	// The full hierarchy names its files too: their names live on the file
	// nodes enrichment writes under them.
	tree, err := repo.ReadHierarchy()
	if err != nil {
		t.Fatal(err)
	}
	named := 0
	for _, ev := range tree.EvidenceFiles {
		for _, p := range ev.Partitions {
			for _, f := range p.Files {
				if f.Name != f.ID+".log" || f.Path != "/"+f.ID+".log" {
					t.Errorf("file %s: name %q, path %q", f.ID, f.Name, f.Path)
				}
				named++
			}
		}
	}
	if named != len(files) {
		t.Fatalf("the hierarchy holds %d files, want %d", named, len(files))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := repo.TopFiles(ctx, 2, 5); err == nil {
		t.Fatal("a cancelled read succeeded")
	}
}
