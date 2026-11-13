package cli

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/internal/progress"
	"indicer/internal/termsafe"
	"indicer/internal/ui"
	"indicer/internal/workpool"
	"indicer/lib/dbopen"
	"indicer/lib/engine"
	"indicer/lib/evidence"
	"indicer/lib/ingest"
	"indicer/lib/logging"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
	"io"
	"os"
	"path/filepath"
	"runtime/trace"
	"time"

	"go.uber.org/zap"
)

// StoreOptions control a store.
type StoreOptions struct {
	NoIndex bool
	// FTS and Enrich run the full-text and graph features after the commit.
	FTS, Enrich bool
	// RevRel and Simhash write each chunk's reverse relation and SimHash
	// signature, which near and its deep mode need.
	RevRel, Simhash bool
	// Decode stores a recognised container (EWF, VHD/VHDX, split raw)
	// decoded (D5). The compression product turns it off: an archive must
	// give back exactly the file it was given.
	Decode bool
	// RequireVerified fails an EWF ingest whose stored digests do not match.
	RequireVerified bool
	// Quiet shows no progress bar: the TUI owns the terminal.
	Quiet bool
	// Jobs is how many evidence of a folder are stored at once; 0 or 1, one
	// at a time (StoreJobs reads --store-jobs).
	Jobs int
	// AutoJobs says Jobs was chosen for this machine (--store-jobs 0): a
	// folder store runs fewer when that many would not fit in memory, where
	// a number the user gave is refused.
	AutoJobs bool
	// maxWorkers caps one evidence's chunk pass while others run beside it.
	maxWorkers int
	// credit is the evidence's place in its folder (store.ChunkCredits).
	credit store.Credit
	// heavy is the decoded size from which a folder's evidence is stored
	// alone; 0 means ingest.HeavySize. Tests lower it.
	heavy int64
}

// StoreData stores the file or folder at evipath. syncIndex is accepted for
// compatibility: the index pass always runs beside storage now.
func StoreData(ctx context.Context, opts DBOptions, evipath string, syncIndex bool, noIndex bool, enableFTS bool, enableEnrichment bool) error {
	return StoreDataWithOptions(ctx, opts, evipath, StoreOptions{NoIndex: noIndex, FTS: enableFTS, Enrich: enableEnrichment, Decode: true})
}

// StoreDataWithOptions stores the file or folder at evipath.
func StoreDataWithOptions(ctx context.Context, opts DBOptions, evipath string, sopts StoreOptions) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sopts.Quiet {
		ctx = progress.Without(ctx)
	}
	start := time.Now()
	logging.GetLogger().Info("StoreData START",
		zap.String("db_path", opts.Path),
		zap.String("evidence_path", evipath),
		zap.Bool("no_index", sopts.NoIndex),
		zap.Bool("enable_fts", sopts.FTS),
		zap.Bool("enable_enrichment", sopts.Enrich),
		zap.Bool("revrel", sopts.RevRel),
		zap.Bool("simhash", sopts.Simhash),
		zap.Bool("decode", sopts.Decode),
	)
	finfo, err := os.Stat(evipath)
	if err != nil {
		logging.GetLogger().Error("StoreData STAT_ERROR", zap.Error(err), zap.String("evidence_path", evipath))
		return err
	}

	h, err := Common(ctx, opts, dbopen.ReadWrite, true)
	if err != nil {
		logging.GetLogger().Error("StoreData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer closeDB(h, &err)
	if err := util.EnsureBlobPath(h.Root); err != nil {
		logging.GetLogger().Error("StoreData ENSURE_BLOB_PATH_ERROR", zap.Error(err), zap.String("db_path", h.Root))
		return err
	}

	// One session for the whole store: the full-text index and the graph
	// open once, when the first evidence needs them, not once per file.
	sess := ingest.NewSession(h.DB)
	defer closeSession(sess, &err)
	if finfo.IsDir() {
		console.Println("Storing Entire Folder")
		err = storeFolder(ctx, console.Out(), sess, evipath, sopts)
	} else {
		_, err = storeFile(ctx, console.Out(), sess, evipath, sopts)
	}
	// What did store is kept, so the value log is compacted either way.
	compactValueLog(ctx, h, "StoreData")
	if err != nil {
		logging.GetLogger().Error("StoreData ERROR", zap.Error(err), zap.String("evidence_path", evipath))
		return err
	}
	opens := sess.Opens()
	logging.GetLogger().Info("StoreData COMPLETE", zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.Int("fts_opens", opens.Text), zap.Int("graph_opens", opens.Graph))
	return nil
}

// storeOneFile stores one item of a folder; tests replace it.
var storeOneFile = storeFile

// itemCost is what one evidence costs beyond its bytes (opening it,
// committing it, starting its index), as bytes of store work: a file of a
// few KiB takes 15 to 65 ms to store, 2 to 8 MiB at 100 to 130 MiB/s. On
// the overall bar the items of a folder or a reindex weigh their size plus
// this, so a large evidence gets the share of the bar it takes and a run of
// small ones still moves it.
const itemCost = 4 << 20

// itemWeight is how much an evidence of size bytes weighs among others.
func itemWeight(size int64) int64 { return max(size, 0) + itemCost }

// storeFolder stores every evidence in a folder, reporting to w. Segment sets
// (EWF, split raw) are stored once, as one evidence; files that are not
// regular are skipped and reported; a file that fails does not stop the rest
// (ING-11). With sopts.Jobs above 1, several evidence are stored at once
// (folderItems says which may not be), and the report reads as a store of
// one at a time would.
func storeFolder(ctx context.Context, w io.Writer, sess *ingest.Session, dir string, sopts StoreOptions) (err error) {
	if sopts.Quiet {
		ctx = progress.Without(ctx)
	}
	eng := engine.For(sess.DB())
	jobs := folderJobs(eng.Tuning(), sopts.Jobs)
	start := time.Now()
	scan := progress.Start(ctx, "scan folder", progress.Files, progress.Unknown)
	plan, err := evidence.Plan(progress.Within(ctx, scan), dir, sopts.Decode)
	scan.Done(err)
	if err != nil {
		return err
	}
	// Which evidence may run beside which, and how many at once fit in
	// memory: each holds its chunk pass and, while indexing, one for an
	// object it stores.
	var items []workpool.Item
	if jobs > 1 {
		var sizes []int64
		items, sizes = folderItems(ctx, sess.DB(), plan.Items, sopts.Decode, jobs, cmp.Or(sopts.heavy, ingest.HeavySize))
		passes := 2
		if sopts.NoIndex {
			passes = 1
		}
		if jobs, err = jobsInMemory(sess.DB(), jobs, sopts.AutoJobs, items, sizes, passes); err != nil {
			return err
		}
	}
	all := progress.Start(ctx, "evidence", progress.Evidence, int64(len(plan.Items)))
	var weight int64
	for _, item := range plan.Items {
		weight += itemWeight(item.Size)
	}
	all.Weigh(weight)
	failed := all.Meter("failed", progress.Evidence)
	defer func() { all.Done(err) }()
	var stored, warned int
	var total ingest.Bytes
	var failures []string
	var firstErr error
	fail := func(path string, err error) {
		failures = append(failures, fmt.Sprintf("%s: %v", path, err))
		if firstErr == nil {
			firstErr = err
		}
	}
	for _, perr := range plan.Errors {
		logging.GetLogger().Error("StoreFolder WALK_ERROR", zap.Error(perr.Err), zap.String("file_path", perr.Path))
		fail(perr.Path, perr.Err)
	}
	for _, s := range plan.Skipped {
		logging.GetLogger().Warn("StoreFolder SKIP", zap.String("file_path", s.Path), zap.String("reason", s.Reason))
		fmt.Fprintf(w, "Skipping %s: %s\n", termsafe.Line(s.Path), s.Reason)
	}
	out := newOrderedOutput(w)
	// The evidence count the chunks they share as a store of one at a time
	// would: new for the first in folder order, whichever stored it.
	credits := store.NewChunkCredits()
	creditOf := func(i int) store.Credit { return store.Credit{Ledger: credits, Item: i} }
	counts := make([]ingest.Bytes, len(plan.Items))
	store := func(ctx context.Context, i int) error {
		item := plan.Items[i]
		name := item.Path
		if rel, rerr := filepath.Rel(dir, item.Path); rerr == nil {
			name = rel
		}
		one := progress.Start(progress.Within(ctx, all), name, progress.None, 0)
		one.Weigh(itemWeight(item.Size))
		isopts := sopts
		if jobs > 1 && !items[i].Heavy {
			// Beside others: a share of the machine. A heavy one runs
			// alone, with all of it.
			isopts.maxWorkers = ingest.InnerWorkers(eng, jobs)
		}
		isopts.credit = creditOf(i)
		probe := startItemProbe(sess.DB(), credits)
		counted, err := storeOneFile(progress.Within(ctx, one), out.of(i), sess, item.Path, isopts)
		probe.done(i, item, jobs > 1 && items[i].Heavy, err)
		one.Done(err)
		all.Add(1)
		counts[i] = counted
		return err
	}
	// report counts evidence i, in folder order. Every evidence before it
	// has ended, so what it stored is final.
	report := func(i int, err error) {
		out.skipTo(i)
		defer out.reported(i)
		defer credits.Settle(i)
		item := plan.Items[i]
		counted := credited(counts[i], creditOf(i))
		var warning *lifecycle.WarningError
		switch {
		case err == nil:
			stored++
			total.Add(counted)
		case errors.As(err, &warning):
			stored++
			warned++
			total.Add(counted)
		case ctx.Err() != nil:
			// Interrupted: the command reports that, not a failed file.
		default:
			logging.GetLogger().Error("StoreFolder STORE_FILE_ERROR", zap.Error(err), zap.String("file_path", item.Path))
			fail(item.Path, err)
			failed.Add(1)
		}
	}
	var pool *workpool.Stats
	if jobs == 1 {
		for i := range plan.Items {
			if err := ctx.Err(); err != nil {
				return err
			}
			report(i, store(ctx, i))
		}
	} else {
		// The ledger holds what evidence stored ahead of others before them;
		// while it is full, only the first unfinished starts.
		st, err := workpool.RunHeld(ctx, jobs, items, credits.Full, store, report)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			return &lifecycle.InternalError{Err: err}
		}
		pool = &st
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "Folder store: %d stored (%d with warnings), %d failed, %d skipped in %v\n", stored, warned, len(failures), len(plan.Skipped), time.Since(start))
	if line := storageLine(total); line != "" {
		fmt.Fprintln(w, " ", line)
	}
	for _, f := range failures {
		fmt.Fprintln(w, "  failed:", termsafe.Line(f))
	}
	took := time.Since(start)
	logging.GetLogger().Info("StoreFolder COMPLETE", append([]zap.Field{
		zap.Int64("duration_ms", took.Milliseconds()),
		zap.Int("stored", stored), zap.Int("warned", warned), zap.Int("failed", len(failures)), zap.Int("skipped", len(plan.Skipped)),
		zap.Int64("read_bytes", total.Read), zap.Int64("new_bytes", total.New), zap.Int64("written_bytes", total.Written),
		zap.Int("jobs", jobs), zap.Int("fts_opens", sess.Opens().Text), zap.Int("graph_opens", sess.Opens().Graph),
	}, poolFields(len(plan.Items), took, pool)...)...)
	switch {
	case len(failures) > 0:
		return fmt.Errorf("%d file(s) in %s failed to store, first %s: %w", len(failures), dir, failures[0], firstErr)
	case warned > 0:
		return &lifecycle.WarningError{Err: fmt.Errorf("%d evidence in %s stored with warnings", warned, dir)}
	}
	return nil
}

// storeFile stores one evidence and says how it ended, to w. It returns the
// bytes its chunk pass read, found new and wrote.
func storeFile(ctx context.Context, w io.Writer, sess *ingest.Session, path string, sopts StoreOptions) (ingest.Bytes, error) {
	// Under --trace, one task per evidence; ingest marks its steps as regions.
	ctx, task := trace.NewTask(ctx, "store evidence")
	defer task.End()
	trace.Log(ctx, "path", path)
	start := time.Now()
	if sopts.credit.Ledger == nil {
		// A run of its own: the evidence's pass still comes before those of
		// the objects its index stores beside it.
		sopts.credit = store.Credit{Ledger: store.NewChunkCredits()}
	}
	logging.GetLogger().Info("StoreFile START", zap.String("file_path", path))
	fmt.Fprintf(w, "\nStoring %s\n", termsafe.Line(path))
	res, err := sess.Run(ctx, ingest.Request{
		Path:            path,
		Decode:          sopts.Decode,
		NoIndex:         sopts.NoIndex,
		FTS:             sopts.FTS,
		Enrich:          sopts.Enrich,
		RevRel:          sopts.RevRel,
		Simhash:         sopts.Simhash,
		RequireVerified: sopts.RequireVerified,
		Quiet:           sopts.Quiet,
		MaxWorkers:      sopts.maxWorkers,
		Credit:          sopts.credit,
	})
	took := time.Since(start)
	// What it stored is final once the evidence before it ended, which is
	// when it is reported and logged (atReport).
	counted := func() ingest.Bytes { return credited(res.Bytes, sopts.credit) }
	if res.Outcome != "" {
		reportIngest(w, res, took, counted)
	}
	if ctx.Err() != nil {
		interruptHint(ctx, res)
	}
	atReport(w, func(io.Writer) {
		b := counted()
		logging.GetLogger().Info("StoreFile COMPLETE",
			zap.String("file_path", path),
			zap.String("outcome", string(res.Outcome)),
			zap.String("evidence_id", base64.StdEncoding.EncodeToString(res.EvidenceID)),
			zap.Int64("duration_ms", took.Milliseconds()),
			zap.Int64("read_bytes", b.Read), zap.Int64("new_bytes", b.New), zap.Int64("written_bytes", b.Written),
			zap.Error(err))
	})
	var incomplete *ingest.IndexIncompleteError
	var mismatch *ingest.DigestMismatchError
	switch {
	case err == nil:
		return res.Bytes, nil
	case errors.As(err, &mismatch) && mismatch.Stored, errors.As(err, &incomplete):
		// Stored, but something needs the examiner's attention: exit 2.
		fmt.Fprintln(w, "Warning:", termsafe.Line(err.Error()))
		return res.Bytes, &lifecycle.WarningError{Err: err}
	}
	return res.Bytes, err
}

// interruptHint tells the examiner what an interrupted store left and how
// to finish it, on the progress footer.
func interruptHint(ctx context.Context, res ingest.Result) {
	switch {
	case res.Outcome == "" && len(res.Attempt) > 0:
		progress.Hint(ctx, "run the same command again to resume: chunks already stored are not stored again")
	case res.Outcome == store.CommitStored:
		id := base64.StdEncoding.EncodeToString(res.EvidenceID)
		progress.Hint(ctx, "the evidence is stored; run `dues reindex "+id+"` to finish its index")
	}
}

// storageLine says what storing took: the bytes read, those new after
// deduplication and what they take on disk, then how much smaller that is:
// "10.0 GiB read, 6.21 GiB new, 2.04 GiB written: dedup 1.61x, compression
// 3.04x, 79.6% saved". It is "" when nothing was read.
func storageLine(b ingest.Bytes) string {
	if b.Read <= 0 {
		return ""
	}
	line := ui.Bytes(b.Read) + " read, " + ui.Bytes(b.New) + " new, " + ui.Bytes(b.Written) + " written: "
	if b.New > 0 {
		line += "dedup " + ui.Ratio(b.Read, b.New) + ", compression " + ui.Ratio(b.New, b.Written) + ", "
	} else {
		line += "all deduplicated, "
	}
	return line + ui.Saved(b.Read, b.Written)
}

// credited is b, what an evidence's chunk pass counted, as a store of one at
// a time would count it: with the chunks a pass before it took, or it took
// from one after (store.ChunkCredits). It is final once every evidence
// before it has ended.
func credited(b ingest.Bytes, c store.Credit) ingest.Bytes {
	n, written := c.Ledger.Adjust(c.Item)
	b.New += n
	b.Written += written
	return b
}

// reportIngest says how an ingest ended, to w. counted is what its storing
// counted (nil: res.Bytes), asked once the report reaches where w writes
// (atReport).
func reportIngest(w io.Writer, res ingest.Result, took time.Duration, counted func() ingest.Bytes) {
	if counted == nil {
		counted = func() ingest.Bytes { return res.Bytes }
	}
	id := base64.StdEncoding.EncodeToString(res.EvidenceID)
	switch res.Outcome {
	case store.CommitAliased:
		fmt.Fprintf(w, "Already stored as evidence %s; recorded this file as an alias of it.\n", id)
		return
	case store.CommitStored:
		if res.Resumed {
			fmt.Fprintf(w, "Resumed an earlier, unfinished ingest of this file.\n")
		}
		fmt.Fprintf(w, "Stored as evidence %s (%d bytes, %s) in %v\n", id, res.Size, res.Source.Format, took)
		atReport(w, func(w io.Writer) {
			if line := storageLine(counted()); line != "" {
				fmt.Fprintln(w, " ", line)
			}
		})
	}
	if c := res.Source.EWF; c != nil {
		fmt.Fprintf(w, "EWF digests: %s\n", c.Result)
		if c.ChunkTablesInvalid > 0 {
			fmt.Fprintf(w, "Warning: %d EWF chunk tables failed their checksum; their chunks were decoded unverified.\n", c.ChunkTablesInvalid)
		}
	}
	if res.Source.Container != "" && res.Source.Format == structs.SourceOpaque {
		fmt.Fprintf(w, "Warning: %s containers cannot be decoded; the file was stored as is and not indexed.\n", res.Source.Container)
	}
	if info := res.Index; info != nil {
		cov := info.Coverage
		switch info.State {
		case structs.IndexSkipped:
			fmt.Fprintf(w, "Not indexed (%s).\n", termsafe.Line(info.Reason))
		default:
			fmt.Fprintf(w, "Index %s: %d partitions, %d of %d files indexed (%d fragmented, %d inline, %d materialised, %d unavailable, %d deleted)\n",
				info.State, cov.Partitions, cov.FilesIndexed, cov.FilesSeen+cov.Symlinks, cov.Fragmented, cov.Inline, cov.Materialized, cov.Unavailable, cov.Deleted)
			if info.Reason != "" && info.State != structs.IndexComplete {
				fmt.Fprintf(w, "  %s\n", termsafe.Line(info.Reason))
			}
		}
	}
}
