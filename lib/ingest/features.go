package ingest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime/trace"

	"indicer/internal/progress"
	"indicer/lib/enrichment"
	"indicer/lib/fts"
	"indicer/lib/index"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// features runs the full-text and graph features over the committed
// evidence's index. They read the stored records, so they run once, after the
// commit, and an alias never runs them twice.
func (s *Session) features(ctx context.Context, eid []byte, req Request) error {
	var errs []error
	if req.FTS {
		region := trace.StartRegion(ctx, "fts")
		ph := progress.Start(ctx, "full-text", progress.Files, progress.Unknown)
		var jobs []fts.IndexedFileJob
		err := index.ForEachIndexed(ctx, s.db, eid, func(key []byte, rec structs.IndexedFile) error {
			jobs = append(jobs, fts.IndexedFileJob{FileID: string(key), Name: rec.Path})
			return nil
		})
		ph.SetTotal(int64(len(jobs)))
		if err == nil && len(jobs) > 0 {
			// Only an evidence with files to index makes the index.
			var x *fts.Index
			if x, err = s.textIndex(true); err == nil {
				_, err = x.IndexFiles(progress.Within(ctx, ph), jobs)
			}
		}
		var unreadable *fts.TextError
		if errors.As(err, &unreadable) {
			// Indexed, with some files' text missing: a warning.
			ph.Warn(unreadable.Error())
			ph.Done(nil)
		} else {
			ph.Done(err)
		}
		region.End()
		if err != nil {
			errs = append(errs, fmt.Errorf("full-text index: %w", err))
		}
	}
	if req.Enrich {
		if err := s.enrich(ctx, eid); err != nil {
			errs = append(errs, fmt.Errorf("graph enrichment: %w", err))
		}
	}
	return errors.Join(errs...)
}

// enrich writes the evidence's hierarchy to the graph. The evidence node is
// named from the record, which carries the path it was stored from.
func (s *Session) enrich(ctx context.Context, eid []byte) error {
	defer trace.StartRegion(ctx, "enrich").End()
	repo, err := s.graphStore(true)
	if err != nil {
		return err
	}
	// The service is not closed: the graph is the session's, until Close.
	_, err = enrichment.NewService(s.db, repo).EnrichEvidenceIndex(ctx, eid)
	return err
}

// DeleteGraphIndex removes what the graph holds of an evidence's index — its
// partitions, indexed files, file nodes and micro-artefacts — so a rebuilt
// index does not sit beside the old one there, and the repair command can
// hide the index of an evidence it quarantines. A database without a graph is
// left without one. keepEvidence keeps the evidence's own node.
func (s *Session) DeleteGraphIndex(eid []byte, keepEvidence bool) error {
	repo, err := s.graphStore(false)
	if err != nil || repo == nil {
		return err
	}
	_, err = repo.DeleteEvidenceIndex(base64.StdEncoding.EncodeToString(eid), keepEvidence)
	return err
}

// DeleteGraphIndex is Session.DeleteGraphIndex in a session of its own.
func DeleteGraphIndex(db *badger.DB, eid []byte, keepEvidence bool) error {
	s := NewSession(db)
	return s.closeWith(s.DeleteGraphIndex(eid, keepEvidence))
}
