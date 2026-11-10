package ingest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"indicer/internal/progress"
	"io"
	"runtime/trace"
	"time"

	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/evidence"
	"indicer/lib/fts"
	"indicer/lib/index"
	"indicer/lib/parser/core"
	"indicer/lib/store"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// ReindexOptions control a reindex.
type ReindexOptions struct {
	// Source reads the evidence from this file instead of its stored chunks.
	// It must hold the same content: its size and fingerprint are checked.
	Source string
	// FTS and Enrich run the features after the pass. A reindex runs the
	// ones the evidence was stored with as well.
	FTS, Enrich bool
	// MaxWorkers caps the chunk pass of each object the index pass stores;
	// 0 leaves the configured width.
	MaxWorkers int
}

// ErrNotReindexable means an evidence cannot be indexed again: it is not
// stored, its index is quarantined, or its content is not a disk DUES reads.
var ErrNotReindexable = errors.New("evidence cannot be reindexed")

// ErrSourceMismatch means a reindex source does not hold the evidence.
var ErrSourceMismatch = errors.New("the source does not hold the evidence's content")

// Reindex rebuilds the index of the stored evidence eid: its previous
// partition and indexed records, their objects and their full-text documents
// are deleted, and the index pass runs again, over the stored chunks (or
// opts.Source). Record IDs are deterministic, so the result is the index a
// first ingest builds (I7). An index that is not complete afterwards is an
// IndexIncompleteError.
func Reindex(ctx context.Context, db *badger.DB, eid []byte, opts ReindexOptions) (Result, error) {
	s := NewSession(db)
	res, err := s.Reindex(ctx, eid, opts)
	return res, s.closeWith(err)
}

// Reindex is the package's Reindex on the session's database.
func (s *Session) Reindex(ctx context.Context, eid []byte, opts ReindexOptions) (Result, error) {
	end, err := s.begin()
	if err != nil {
		return Result{}, err
	}
	defer end()
	db := s.db
	e, err := dbio.GetEvidenceFile(eid, db)
	if err != nil {
		return Result{}, err
	}
	res := Result{EvidenceID: eid, Outcome: store.CommitStored, Size: e.Size}
	if e.Source != nil {
		res.Source = *e.Source
	}
	switch {
	case !e.Completed:
		return res, fmt.Errorf("%w: evidence %s is not stored (%s)", ErrNotReindexable, b64(eid), e.IngestState)
	case e.Index != nil && e.Index.State == structs.IndexInvalid:
		return res, fmt.Errorf("%w: evidence %s %s", ErrNotReindexable, b64(eid), e.Index.Reason)
	case e.Source != nil && e.Source.Format == structs.SourceOpaque:
		return res, fmt.Errorf("%w: evidence %s is a %s container DUES cannot decode", ErrNotReindexable, b64(eid), e.Source.Container)
	case e.Size == 0:
		return res, fmt.Errorf("%w: evidence %s is empty", ErrNotReindexable, b64(eid))
	}

	img, closeImg, err := reindexImage(ctx, db, eid, e, opts.Source)
	if err != nil {
		return res, err
	}
	defer closeImg()

	// Decoded content the index stores gets what the evidence was stored
	// with. Evidence stored before W6 records no features; its chunks are
	// asked instead.
	revrel, err := store.HasReverseRelations(db, eid, e)
	if err != nil {
		return res, err
	}
	simhash, err := store.HasSignatures(db, eid, e)
	if err != nil {
		return res, err
	}
	req := Request{FTS: opts.FTS || hasFeature(e, "fts"), Enrich: opts.Enrich || hasFeature(e, "enrich"), RevRel: revrel, Simhash: simhash}
	progress.PlanSteps(ctx, append([]progress.Step{
		{Name: "delete old index", Unit: progress.Files, Total: progress.Unknown},
		{Name: "index", Unit: progress.Files, Total: progress.Unknown},
	}, featureSteps(req, e.Size)...)...)

	// The previous index goes first, full-text documents with it, so records
	// a changed parser no longer finds do not linger.
	if err := s.deletePrevious(ctx, eid); err != nil {
		return res, err
	}

	info := &structs.IndexInfo{State: structs.IndexRunning, Started: time.Now().UnixNano()}
	if err := setIndexInfo(db, eid, info); err != nil {
		return res, err
	}
	region := trace.StartRegion(ctx, "index")
	idx, idxErr := s.passes.index(ctx, index.Options{
		DB: db, EvidenceID: eid, EvidenceName: e.Name, Image: img, Entropy: req.Enrich,
		RevRel: req.RevRel, Simhash: req.Simhash, MaxWorkers: opts.MaxWorkers,
	})
	region.End()
	res.Partitions = idx.Partitions
	info.Coverage, info.Finished = idx.Coverage, time.Now().UnixNano()
	switch {
	case idxErr != nil:
		info.State, info.Reason = structs.IndexFailed, idxErr.Error()
	default:
		info.State, info.Reason = idx.State, idx.Reason
		if err := reindexPartitionHashes(ctx, db, e, img, idx.Partitions); err != nil {
			info.State, info.Reason, idxErr = structs.IndexFailed, err.Error(), err
		}
	}
	if info.State == structs.IndexComplete || info.State == structs.IndexPartial {
		if err := s.features(ctx, eid, req); err != nil {
			info.State, info.Reason, idxErr = structs.IndexPartial, err.Error(), err
		}
	} else {
		skipFeatures(ctx, req, "index "+string(info.State))
	}
	if err := setIndexInfo(db, eid, info); err != nil {
		return res, err
	}
	res.Index = info
	if info.State != structs.IndexComplete {
		return res, &IndexIncompleteError{EvidenceID: eid, State: info.State, Reason: info.Reason, Err: idxErr}
	}
	return res, nil
}

func b64(id []byte) string { return base64.StdEncoding.EncodeToString(id) }

func hasFeature(e structs.EvidenceFile, f string) bool {
	for _, x := range e.Features {
		if x == f {
			return true
		}
	}
	return false
}

// reindexImage opens what the index pass reads: the evidence's stored chunks,
// each checked against its hash, or a source file holding the same content.
func reindexImage(ctx context.Context, db *badger.DB, eid []byte, e structs.EvidenceFile, source string) (core.Image, func(), error) {
	if source == "" {
		ec, err := dbio.OpenEvidenceChunks(db, eid, dbio.ReadOptions{})
		if err != nil {
			return nil, nil, err
		}
		return storedImage{ec: ec, size: e.Size}, func() {}, nil
	}
	decode := e.Source == nil || e.Source.Format != structs.SourceRaw || e.Source.Container == ""
	src, err := evidence.Open(ctx, source, evidence.Options{Decode: decode})
	if err != nil {
		return nil, nil, err
	}
	fp, err := src.Fingerprint(dbmeta.FileHasher(db))
	switch {
	case err != nil:
		src.Close()
		return nil, nil, err
	case src.Size() != e.Size || (e.Fingerprint != "" && fp != e.Fingerprint):
		src.Close()
		return nil, nil, fmt.Errorf("%w: %s (%d bytes)", ErrSourceMismatch, source, src.Size())
	}
	return src.Image(), func() { src.Close() }, nil
}

// storedImage is a stored evidence read as the image the scan walks.
type storedImage struct {
	ec   *dbio.EvidenceChunks
	size int64
}

func (s storedImage) ReadAt(p []byte, off int64) (int, error) { return s.ec.ReadAt(p, off) }
func (s storedImage) Size() int64                             { return s.size }
func (s storedImage) Format() core.ImageFormat                { return core.FormatRaw }
func (s storedImage) Close() error                            { return nil }

// deletePrevious removes an evidence's previous index: its records, their
// full-text documents and its graph nodes, which a rebuilt index would
// otherwise sit beside.
func (s *Session) deletePrevious(ctx context.Context, eid []byte) (err error) {
	ph := progress.Start(ctx, "delete old index", progress.Files, progress.Unknown)
	defer func() { ph.Done(err) }()
	var docs []string
	if err := store.DeleteEvidenceIndex(ctx, s.db, eid, func(key []byte) {
		docs = append(docs, string(key))
		ph.Add(1)
	}); err != nil {
		return fmt.Errorf("delete the previous index: %w", err)
	}
	if len(docs) > 0 {
		// A database with no full-text index has no documents to delete.
		x, err := s.textIndex(false)
		if err == nil {
			err = x.DeleteIndexedFiles(docs)
		}
		if err != nil && !errors.Is(err, fts.ErrIndexNotReady) {
			return fmt.Errorf("delete the previous full-text documents: %w", err)
		}
	}
	if err := s.deleteGraphIndex(eid, true); err != nil {
		return fmt.Errorf("delete the previous index from the graph: %w", err)
	}
	return nil
}

// reindexPartitionHashes hashes each partition's range of the image, except a
// filesystem spanning the whole device, which is the evidence's own hash.
func reindexPartitionHashes(ctx context.Context, db *badger.DB, e structs.EvidenceFile, img core.Image, parts []index.Partition) (err error) {
	defer trace.StartRegion(ctx, "partition-hashes").End()
	plain, _ := base64.StdEncoding.DecodeString(e.PlainHash)
	usable := func(p index.Partition) bool { return p.Size > 0 && p.Offset >= 0 && p.Size <= img.Size()-p.Offset }
	whole := func(p index.Partition) bool { return p.Offset == 0 && p.Size == img.Size() && len(plain) > 0 }
	var need int64
	for _, p := range parts {
		if usable(p) && !whole(p) {
			need += p.Size
		}
	}
	var ph *progress.Phase
	if need > 0 {
		ph = progress.Start(ctx, "partition hashes", progress.Bytes, need)
		defer func() { ph.Done(err) }()
	}
	for _, p := range parts {
		if !usable(p) {
			continue
		}
		h := plain
		if !whole(p) {
			sums, err := store.HashContent(progress.Within(ctx, ph), db, io.NewSectionReader(img, p.Offset, p.Size), p.Size, false)
			if err != nil {
				return fmt.Errorf("hash partition at %d: %w", p.Offset, err)
			}
			h = sums.Plain
		}
		if err := index.SetPartitionHash(db, p.ID, h); err != nil {
			return err
		}
	}
	return nil
}
