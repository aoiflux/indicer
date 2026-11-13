package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"indicer/internal/progress"
	"io"
	"runtime/trace"
	"time"

	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/internal/termsafe"
	"indicer/internal/workpool"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbopen"
	"indicer/lib/engine"
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
	// Jobs is how many evidence are reindexed at once (Jobs reads
	// --store-jobs); 0 or 1 reindexes one at a time.
	Jobs int
	// AutoJobs says Jobs was chosen for this machine: fewer run when that
	// many would not fit in memory, where a number the user gave is refused.
	AutoJobs bool
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
	_, err = reindexEvidence(ctx, h.DB, ro, console.Out())
	compactValueLog(ctx, h, "ReindexData")
	return err
}

type reindexTarget struct {
	id   []byte
	name string
	size int64
}

// reindexOutcome is how the reindex of one evidence ended.
type reindexOutcome struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Outcome string `json:"outcome"` // ok, incomplete or failed
	Error   string `json:"error,omitempty"`
}

// reindexSummary is what reindexEvidence did; repair prints it in its
// document.
type reindexSummary struct {
	Evidence   []reindexOutcome `json:"evidence"`
	Incomplete int              `json:"incomplete"`
	Failed     int              `json:"failed"`
}

// reindexEvidence reindexes each selected evidence, reporting on w. With
// ro.Jobs above 1, several are reindexed at once, and the report reads as a
// reindex of one at a time would. One that fails does not stop the rest; an
// index left incomplete exits 2.
func reindexEvidence(ctx context.Context, db *badger.DB, ro ReindexOptions, w io.Writer) (sum reindexSummary, err error) {
	began := time.Now()
	sum.Evidence = []reindexOutcome{}
	targets, err := reindexTargets(db, ro)
	if err != nil {
		return sum, err
	}
	if ro.Source != "" && len(targets) != 1 {
		return sum, lifecycle.Usagef("--source reads one evidence; %d are selected", len(targets))
	}
	if len(targets) == 0 {
		fmt.Fprintln(w, "Nothing to reindex.")
		return sum, nil
	}
	// Evidence share nothing a reindex deletes or writes: records, objects,
	// full-text documents and graph nodes are keyed by their evidence's own
	// ID. So none are chained; a large one runs alone, and the others share
	// the machine and its memory: each holds the chunk pass of an object its
	// index pass stores.
	eng := engine.For(db)
	jobs := folderJobs(eng.Tuning(), ro.Jobs)
	items := make([]workpool.Item, len(targets))
	sizes := make([]int64, len(targets))
	for i, t := range targets {
		items[i].Heavy, sizes[i] = t.size >= ingest.HeavySize, t.size
	}
	if jobs, err = jobsInMemory(db, jobs, ro.AutoJobs, items, sizes, 1); err != nil {
		return sum, err
	}
	// One session for the whole reindex: the full-text index and the graph
	// open once, not once per evidence.
	sess := ingest.NewSession(db)
	defer closeSession(sess, &err)
	var failures []string
	var firstErr error
	all := progress.Start(ctx, "evidence", progress.Evidence, int64(len(targets)))
	var weight int64
	for _, t := range targets {
		weight += itemWeight(t.size)
	}
	all.Weigh(weight)
	failed := all.Meter("failed", progress.Evidence)
	defer func() { all.Done(err) }()
	out := newOrderedOutput(w)
	reindex := func(ctx context.Context, i int) error {
		ctx, task := trace.NewTask(ctx, "reindex evidence")
		defer task.End()
		t := targets[i]
		iw := out.of(i)
		start := time.Now()
		id := base64.StdEncoding.EncodeToString(t.id)
		trace.Log(ctx, "evidence_id", id)
		fmt.Fprintf(iw, "\nReindexing %s (%s)\n", termsafe.Line(t.name), id)
		one := progress.Start(progress.Within(ctx, all), t.name, progress.None, 0)
		one.Weigh(itemWeight(t.size))
		opts := ingest.ReindexOptions{Source: ro.Source, FTS: ro.FTS, Enrich: ro.Enrich}
		if jobs > 1 && !items[i].Heavy {
			opts.MaxWorkers = ingest.InnerWorkers(eng, jobs)
		}
		res, err := sess.Reindex(progress.Within(ctx, one), t.id, opts)
		one.Done(err)
		all.Add(1)
		if res.Index != nil {
			reportIngest(iw, res, time.Since(start), nil)
		}
		logging.GetLogger().Info("Reindex EVIDENCE",
			zap.String("evidence_id", id),
			zap.Int64("duration_ms", time.Since(start).Milliseconds()),
			zap.Error(err))
		return err
	}
	// report counts evidence i, in the order they were selected.
	report := func(i int, err error) {
		out.skipTo(i)
		defer out.reported(i)
		t := targets[i]
		iw := out.of(i)
		o := reindexOutcome{ID: base64.StdEncoding.EncodeToString(t.id), Name: t.name, Outcome: "ok"}
		var incomplete *ingest.IndexIncompleteError
		switch {
		case err == nil:
		case errors.As(err, &incomplete):
			o.Outcome, o.Error = "incomplete", err.Error()
			sum.Incomplete++
			fmt.Fprintln(iw, "Warning:", termsafe.Line(err.Error()))
		case ctx.Err() != nil:
			// Interrupted: the command reports that, not a failed evidence.
			return
		default:
			o.Outcome, o.Error = "failed", err.Error()
			sum.Failed++
			failures = append(failures, fmt.Sprintf("%s: %v", t.name, err))
			failed.Add(1)
			if firstErr == nil {
				firstErr = err
			}
			fmt.Fprintln(iw, "Failed:", termsafe.Line(err.Error()))
		}
		sum.Evidence = append(sum.Evidence, o)
	}
	var pool *workpool.Stats
	if jobs == 1 {
		for i := range targets {
			if err := ctx.Err(); err != nil {
				return sum, err
			}
			report(i, reindex(ctx, i))
		}
	} else {
		st, err := workpool.Run(ctx, jobs, items, reindex, report)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return sum, cerr
			}
			return sum, &lifecycle.InternalError{Err: err}
		}
		pool = &st
	}
	if err := ctx.Err(); err != nil {
		return sum, err
	}
	took := time.Since(began)
	logging.GetLogger().Info("Reindex COMPLETE", append([]zap.Field{
		zap.Int64("duration_ms", took.Milliseconds()),
		zap.Int("incomplete", sum.Incomplete), zap.Int("failed", sum.Failed),
		zap.Int("jobs", jobs), zap.Int("fts_opens", sess.Opens().Text), zap.Int("graph_opens", sess.Opens().Graph),
	}, poolFields(len(targets), took, pool)...)...)
	if len(targets) > 1 {
		fmt.Fprintf(w, "\nReindex: %d evidence, %d with an incomplete index, %d failed\n", len(targets), sum.Incomplete, sum.Failed)
	}
	switch {
	case sum.Failed > 0:
		return sum, fmt.Errorf("%d of %d evidence could not be reindexed, first %s: %w", sum.Failed, len(targets), failures[0], firstErr)
	case sum.Incomplete > 0:
		return sum, &lifecycle.WarningError{Err: fmt.Errorf("%d evidence reindexed with an incomplete index", sum.Incomplete)}
	}
	return sum, nil
}

// reindexTargets resolves what ro selects, each evidence once.
func reindexTargets(db *badger.DB, ro ReindexOptions) ([]reindexTarget, error) {
	var out []reindexTarget
	seen := map[string]bool{}
	add := func(id []byte, e structs.EvidenceFile) {
		if !seen[string(id)] {
			seen[string(id)] = true
			out = append(out, reindexTarget{id: id, name: e.Name, size: e.Size})
		}
	}
	for _, ref := range ro.IDs {
		id, e, err := resolveEvidence(db, ref)
		if err != nil {
			return nil, err
		}
		add(id, e)
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
				add(id, e)
			case state == structs.IndexFailed || state == structs.IndexInterrupted:
				add(id, e)
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
