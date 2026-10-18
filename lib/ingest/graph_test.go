package ingest

import (
	"encoding/base64"
	"io"
	"math"
	"os"
	"testing"

	"indicer/lib/dbio"
	"indicer/lib/enrichment"
	"indicer/lib/entropy"
	duesgraph "indicer/lib/graph"
	"indicer/lib/graph/props"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

func graphProps(t *testing.T, root, key, value string) []props.Map {
	t.Helper()
	h, err := duesgraph.OpenWith(root, duesgraph.OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	ids, err := h.Graph().NodesByProperty(key, []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	var out []props.Map
	for _, id := range ids {
		node, err := h.Graph().GetNode(id)
		if err != nil {
			t.Fatal(err)
		}
		fields, err := props.Unpack(node.Properties)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fields)
	}
	return out
}

func closeTo(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestEnrichmentGraph: an ingest with enrichment writes the evidence, its
// partitions and its files to the graph with the entropy of their content
// (files from the index pass, the evidence and partitions from one read of
// the evidence), names the evidence by the path it was stored from, and a
// reindex leaves no node of an index that no longer exists.
func TestEnrichmentGraph(t *testing.T) {
	db := openDB(t)
	path := fixture(t, "part/gpt_multi")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(t.Context(), db, Request{Path: path, Decode: true, Quiet: true, Enrich: true})
	if err != nil {
		t.Fatal(err)
	}
	eid := res.EvidenceID
	evB64 := base64.StdEncoding.EncodeToString(eid)
	root := db.Opts().Dir

	ev := graphProps(t, root, "evidence_file_id", evB64)
	if len(ev) != 1 || ev[0].Str("path") != path {
		t.Fatalf("evidence nodes %v, want one with path %s", ev, path)
	}
	evFile := graphProps(t, root, "level", "evidence_file")
	if len(evFile) != 1 || !closeTo(evFile[0].Float64("entropy"), entropy.Of(source)) {
		t.Fatalf("evidence file node %v; the evidence's entropy is %v", evFile, entropy.Of(source))
	}

	e, err := dbio.GetEvidenceFile(eid, db)
	if err != nil {
		t.Fatal(err)
	}
	parts := 0
	for pidB64 := range e.InternalObjects {
		pid, _ := base64.StdEncoding.DecodeString(pidB64)
		p, err := dbio.GetPartitionFile(pid, db)
		if err != nil {
			t.Fatal(err)
		}
		want := entropy.Of(source[p.Start : p.Start+p.Size])
		found := false
		for _, f := range graphProps(t, root, "level", "partition") {
			if f.Str("partition_id") == pidB64 {
				found = true
				if !f.Bool("has_entropy") || !closeTo(f.Float64("entropy"), want) {
					t.Errorf("partition %s: entropy %v (has %v), its range's is %v", p.Path, f.Float64("entropy"), f.Bool("has_entropy"), want)
				}
			}
		}
		if !found {
			t.Errorf("partition %s has no file node", p.Path)
		}
		parts++

		err = dbio.ForEachPartitionMember(db, pid, p, func(iidB64 string, _ structs.InternalOffset) error {
			iid, _ := base64.StdEncoding.DecodeString(iidB64)
			rec, err := dbio.GetIndexedFile(iid, db)
			if err != nil {
				return err
			}
			if rec.Entropy == nil {
				t.Errorf("%s: the index pass recorded no entropy", rec.Path)
				return nil
			}
			content := readContent(t, db, iid)
			if !closeTo(*rec.Entropy, entropy.Of(content)) {
				t.Errorf("%s: recorded entropy %v, its content's %v", rec.Path, *rec.Entropy, entropy.Of(content))
			}
			nodes := graphProps(t, root, "indexed_file_id", iidB64)
			if len(nodes) != 1 || !closeTo(nodes[0].Float64("entropy"), *rec.Entropy) {
				t.Errorf("%s: graph nodes %v", rec.Path, nodes)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if parts == 0 {
		t.Fatal("no partitions")
	}

	// A partition and file the index no longer has, as a reindex finds after
	// an index written by an earlier release.
	repo, err := enrichment.OpenGrapheneRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	stale := enrichment.FileRecord{
		Level: "indexed_file", EvidenceFileID: evB64, EvidenceFileName: e.Name,
		PartitionID: "stale-partition", PartitionName: "old", IndexedFileID: "stale-file",
		FileName: "gone.txt", Path: "/gone.txt",
	}
	if err := repo.UpsertFile(stale); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Reindex(t.Context(), db, eid, ReindexOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{{"partition_id", "stale-partition"}, {"indexed_file_id", "stale-file"}} {
		if got := graphProps(t, root, kv[0], kv[1]); len(got) != 0 {
			t.Errorf("after a reindex the graph still holds %s=%s", kv[0], kv[1])
		}
	}
	if got := graphProps(t, root, "evidence_file_id", evB64); len(got) != 1 {
		t.Errorf("after a reindex, %d evidence nodes", len(got))
	}
	if got := graphProps(t, root, "level", "indexed_file"); len(got) == 0 {
		t.Error("the reindex did not enrich the rebuilt index")
	}
}

func readContent(t *testing.T, db *badger.DB, iid []byte) []byte {
	t.Helper()
	lg, err := dbio.OpenLogical(db, dbio.CanonicalIndexedKey(iid), dbio.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(io.NewSectionReader(lg, 0, lg.Size()))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
