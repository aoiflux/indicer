package ingest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"indicer/lib/enrichment"
	"indicer/lib/fts"
	duesgraph "indicer/lib/graph"
	"indicer/lib/index"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// features runs the full-text and graph features over the committed
// evidence's index. They read the stored records, so they run once, after the
// commit, and an alias never runs them twice.
func features(ctx context.Context, db *badger.DB, eid []byte, req Request) error {
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
		if err := enrich(ctx, db, eid); err != nil {
			errs = append(errs, fmt.Errorf("graph enrichment: %w", err))
		}
	}
	return errors.Join(errs...)
}

// enrich writes the evidence's hierarchy to the graph. The evidence node is
// named from the record, which carries the path it was stored from.
func enrich(ctx context.Context, db *badger.DB, eid []byte) (err error) {
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
	_, err = svc.EnrichEvidenceIndex(ctx, eid)
	return err
}

// deleteGraphIndex removes what the graph holds of an evidence's index — its
// partitions, indexed files, file nodes and micro-artefacts — so a rebuilt
// index does not sit beside the old one there. A database without a graph is
// left without one. keepEvidence keeps the evidence's own node.
func deleteGraphIndex(db *badger.DB, eid []byte, keepEvidence bool) (err error) {
	root := db.Opts().Dir
	if !duesgraph.Exists(root) {
		return nil
	}
	repo, err := enrichment.OpenGrapheneRepository(root)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := repo.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = repo.DeleteEvidenceIndex(base64.StdEncoding.EncodeToString(eid), keepEvidence)
	return err
}

// DeleteGraphIndex is deleteGraphIndex for the repair command, which hides
// the index of an evidence it quarantines.
func DeleteGraphIndex(db *badger.DB, eid []byte, keepEvidence bool) error {
	return deleteGraphIndex(db, eid, keepEvidence)
}
