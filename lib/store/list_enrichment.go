package store

import (
	"encoding/json"
	"indicer/internal/console"

	"indicer/lib/enrichment"

	"github.com/dgraph-io/badger/v4"
)

// IndexedAnnotator returns what graph enrichment derived for each indexed
// file, by indexed-file ID.
type IndexedAnnotator interface {
	IndexedFileAnnotations() (map[string]enrichment.FileAnnotation, error)
}

// ListWithEnrichment lists from the database, which is authoritative, and
// adds to each indexed file what graph enrichment derived for it: its MIME
// type, tags and entropy.
//
// It used to list the evidence the graph held and fill in from the database,
// so evidence stored without enrichment was missing from the listing, and the
// graph's view of an evidence stood in for the database's (GRA-24).
func ListWithEnrichment(db *badger.DB, graph IndexedAnnotator, statusFilter string) error {
	out, err := listOutput(db, statusFilter)
	if err != nil {
		return err
	}
	annotations, err := graph.IndexedFileAnnotations()
	if err != nil {
		return err
	}
	annotateIndexedFiles(out, annotations)
	jsonData, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	console.Println(string(jsonData))
	return nil
}

func annotateIndexedFiles(out map[string]interface{}, annotations map[string]enrichment.FileAnnotation) {
	evidence, _ := out["evidence"].([]map[string]interface{})
	for _, e := range evidence {
		partitions, _ := e["partitions"].([]map[string]interface{})
		for _, p := range partitions {
			files, _ := p["indexedFiles"].([]map[string]interface{})
			for _, f := range files {
				id, _ := f["id"].(string)
				a, ok := annotations[id]
				if !ok {
					continue
				}
				f["mimeType"] = a.MimeType
				f["tags"] = a.Tags
				if a.HasEntropy {
					f["entropy"] = a.Entropy
				}
			}
		}
	}
}
