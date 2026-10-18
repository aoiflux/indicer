package ingest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"indicer/lib/enrichment"
	"indicer/lib/fts"
	"indicer/lib/index"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// features runs the full-text and graph features over the committed
// evidence's index. They read the stored records, so they run once, after the
// commit, and an alias never runs them twice.
func features(ctx context.Context, db *badger.DB, eid []byte, node evidenceNode, req Request) error {
	var errs []error
	if req.FTS {
		var jobs []fts.IndexedFileJob
		err := index.ForEachIndexed(ctx, db, eid, func(key []byte, rec structs.IndexedFile) error {
			jobs = append(jobs, fts.IndexedFileJob{FileID: string(key), Name: rec.Path})
			return nil
		})
		if err == nil && len(jobs) > 0 {
			_, err = fts.IndexFiles(ctx, db, jobs)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("full-text index: %w", err))
		}
	}
	if req.Enrich {
		if err := enrich(db, eid, node); err != nil {
			errs = append(errs, fmt.Errorf("graph enrichment: %w", err))
		}
	}
	return errors.Join(errs...)
}

// evidenceNode is what the graph's evidence node records.
type evidenceNode struct {
	Name, Path string
	Size       int64
}

func enrich(db *badger.DB, eid []byte, node evidenceNode) (err error) {
	repo, err := enrichment.OpenGrapheneRepository(db.Opts().Dir)
	if err != nil {
		return err
	}
	svc := enrichment.NewService(db, repo)
	defer func() {
		if cerr := svc.Close(); err == nil {
			err = cerr
		}
	}()
	err = svc.EnrichEvidence(enrichment.EvidenceRecord{
		HashBase64: base64.StdEncoding.EncodeToString(eid),
		Name:       node.Name,
		Path:       node.Path,
		Size:       node.Size,
	})
	if err != nil {
		return err
	}
	return svc.EnrichEvidenceIndex(eid)
}
