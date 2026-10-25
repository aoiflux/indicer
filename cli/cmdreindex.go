package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/internal/termsafe"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbopen"
	"indicer/lib/ingest"
	"indicer/lib/logging"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
)

// ReindexOptions select the evidence reindex rebuilds.
type ReindexOptions struct {
	// IDs are evidence IDs or hashes, as list shows them.
	IDs []string
	// Failed selects every evidence whose index pass failed or was
	// interrupted; All, every stored evidence.
	Failed, All bool
	// Source reads one evidence from this file instead of its stored chunks.
	Source string
	// FTS and Enrich also run those features.
	FTS, Enrich bool
}

// ReindexData rebuilds the indexes ro selects.
func ReindexData(ctx context.Context, opts DBOptions, ro ReindexOptions) (err error) {
	if len(ro.IDs) == 0 && !ro.Failed && !ro.All {
		return lifecycle.Usagef("name the evidence to reindex, or pass --failed or --all")
	}
	h, err := Common(ctx, opts, dbopen.ReadWrite, true)
	if err != nil {
		return err
	}
	defer closeDB(h, &err)
	err = reindexEvidence(ctx, h.DB, ro)
	compactValueLog(h, "ReindexData")
	return err
}

type reindexTarget struct {
	id   []byte
	name string
}

// reindexEvidence reindexes each selected evidence in turn. One that fails
// does not stop the rest; an index left incomplete exits 2.
func reindexEvidence(ctx context.Context, db *badger.DB, ro ReindexOptions) error {
	targets, err := reindexTargets(db, ro)
	if err != nil {
		return err
	}
	if ro.Source != "" && len(targets) != 1 {
		return lifecycle.Usagef("--source reads one evidence; %d are selected", len(targets))
	}
	if len(targets) == 0 {
		console.Println("Nothing to reindex.")
		return nil
	}
	var warned int
	var failures []string
	var firstErr error
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		start := time.Now()
		console.Printf("\nReindexing %s (%s)\n", termsafe.Line(t.name), base64.StdEncoding.EncodeToString(t.id))
		res, err := ingest.Reindex(ctx, db, t.id, ingest.ReindexOptions{Source: ro.Source, FTS: ro.FTS, Enrich: ro.Enrich})
		if res.Index != nil {
			reportIngest(console.Out(), res, time.Since(start))
		}
		logging.GetLogger().Info("Reindex EVIDENCE",
			zap.String("evidence_id", base64.StdEncoding.EncodeToString(t.id)),
			zap.Int64("duration_ms", time.Since(start).Milliseconds()),
			zap.Error(err))
		var incomplete *ingest.IndexIncompleteError
		switch {
		case err == nil:
		case errors.As(err, &incomplete):
			warned++
			console.Println("Warning:", termsafe.Line(err.Error()))
		default:
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			failures = append(failures, fmt.Sprintf("%s: %v", t.name, err))
			if firstErr == nil {
				firstErr = err
			}
			console.Println("Failed:", termsafe.Line(err.Error()))
		}
	}
	if len(targets) > 1 {
		console.Printf("\nReindex: %d evidence, %d with an incomplete index, %d failed\n", len(targets), warned, len(failures))
	}
	switch {
	case len(failures) > 0:
		return fmt.Errorf("%d of %d evidence could not be reindexed, first %s: %w", len(failures), len(targets), failures[0], firstErr)
	case warned > 0:
		return &lifecycle.WarningError{Err: fmt.Errorf("%d evidence reindexed with an incomplete index", warned)}
	}
	return nil
}

// reindexTargets resolves what ro selects, each evidence once.
func reindexTargets(db *badger.DB, ro ReindexOptions) ([]reindexTarget, error) {
	var out []reindexTarget
	seen := map[string]bool{}
	add := func(id []byte, name string) {
		if !seen[string(id)] {
			seen[string(id)] = true
			out = append(out, reindexTarget{id: id, name: name})
		}
	}
	for _, ref := range ro.IDs {
		id, e, err := resolveEvidence(db, ref)
		if err != nil {
			return nil, err
		}
		add(id, e.Name)
	}
	if !ro.Failed && !ro.All {
		return out, nil
	}
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(cnst.EviFileNamespace)
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			id := dbio.EvidenceRawID(it.Item().KeyCopy(nil))
			e, err := dbio.GetEvidenceTxn(txn, id)
			if err != nil {
				return err
			}
			if !e.Completed || e.Size == 0 {
				continue
			}
			state := structs.IndexState("")
			if e.Index != nil {
				state = e.Index.State
			}
			switch {
			case state == structs.IndexInvalid:
			case e.Source != nil && e.Source.Format == structs.SourceOpaque:
			case ro.All:
				add(id, e.Name)
			case state == structs.IndexFailed || state == structs.IndexInterrupted:
				add(id, e.Name)
			}
		}
		return nil
	})
	return out, err
}

// resolveEvidence finds the evidence ref names: its ID, or a hash a lookup
// gives it under.
func resolveEvidence(db *badger.DB, ref string) ([]byte, structs.EvidenceFile, error) {
	if raw, err := base64.StdEncoding.DecodeString(ref); err == nil && len(raw) > 0 {
		if e, err := dbio.GetEvidenceFile(raw, db); err == nil {
			return raw, e, nil
		}
		if keys, err := dbio.GuessFileTypes(ref, db); err == nil {
			for _, k := range keys {
				if bytes.HasPrefix(k, []byte(cnst.EviFileNamespace)) {
					id := dbio.EvidenceRawID(k)
					e, err := dbio.GetEvidenceFile(id, db)
					return id, e, err
				}
			}
		}
	}
	return nil, structs.EvidenceFile{}, lifecycle.Usagef("no stored evidence %q", ref)
}
