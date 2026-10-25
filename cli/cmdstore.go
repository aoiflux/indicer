package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/internal/termsafe"
	"indicer/lib/dbopen"
	"indicer/lib/evidence"
	"indicer/lib/ingest"
	"indicer/lib/logging"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
	"io"
	"os"
	"time"

	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
)

// StoreOptions control a store.
type StoreOptions struct {
	NoIndex bool
	// FTS and Enrich run the full-text and graph features after the commit.
	FTS, Enrich bool
	// Decode stores a recognised container (EWF, VHD/VHDX, split raw)
	// decoded (D5). The compression product turns it off: an archive must
	// give back exactly the file it was given.
	Decode bool
	// RequireVerified fails an EWF ingest whose stored digests do not match.
	RequireVerified bool
	// Quiet shows no progress bar: the TUI owns the terminal.
	Quiet bool
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
	start := time.Now()
	logging.GetLogger().Info("StoreData START",
		zap.String("db_path", opts.Path),
		zap.String("evidence_path", evipath),
		zap.Bool("no_index", sopts.NoIndex),
		zap.Bool("enable_fts", sopts.FTS),
		zap.Bool("enable_enrichment", sopts.Enrich),
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

	if finfo.IsDir() {
		console.Println("Storing Entire Folder")
		err = storeFolder(ctx, console.Out(), h.DB, evipath, sopts)
	} else {
		err = storeFile(ctx, console.Out(), h.DB, evipath, sopts)
	}
	// What did store is kept, so the value log is compacted either way.
	compactValueLog(h, "StoreData")
	if err != nil {
		logging.GetLogger().Error("StoreData ERROR", zap.Error(err), zap.String("evidence_path", evipath))
		return err
	}
	logging.GetLogger().Info("StoreData COMPLETE", zap.Int64("duration_ms", time.Since(start).Milliseconds()))
	return nil
}

// storeOneFile stores one item of a folder; tests replace it.
var storeOneFile = storeFile

// storeFolder stores every evidence in a folder, reporting to w. Segment sets
// (EWF, split raw) are stored once, as one evidence; files that are not
// regular are skipped and reported; a file that fails does not stop the rest
// (ING-11).
func storeFolder(ctx context.Context, w io.Writer, db *badger.DB, dir string, sopts StoreOptions) error {
	start := time.Now()
	plan, err := evidence.Plan(ctx, dir, sopts.Decode)
	if err != nil {
		return err
	}
	var stored, warned int
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
	for _, item := range plan.Items {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := storeOneFile(ctx, w, db, item.Path, sopts)
		var warning *lifecycle.WarningError
		switch {
		case err == nil:
			stored++
		case errors.As(err, &warning):
			stored++
			warned++
		default:
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			logging.GetLogger().Error("StoreFolder STORE_FILE_ERROR", zap.Error(err), zap.String("file_path", item.Path))
			fail(item.Path, err)
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "Folder store: %d stored (%d with warnings), %d failed, %d skipped in %v\n", stored, warned, len(failures), len(plan.Skipped), time.Since(start))
	for _, f := range failures {
		fmt.Fprintln(w, "  failed:", termsafe.Line(f))
	}
	logging.GetLogger().Info("StoreFolder COMPLETE",
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.Int("stored", stored), zap.Int("warned", warned), zap.Int("failed", len(failures)), zap.Int("skipped", len(plan.Skipped)))
	switch {
	case len(failures) > 0:
		return fmt.Errorf("%d file(s) in %s failed to store, first %s: %w", len(failures), dir, failures[0], firstErr)
	case warned > 0:
		return &lifecycle.WarningError{Err: fmt.Errorf("%d evidence in %s stored with warnings", warned, dir)}
	}
	return nil
}

// storeFile stores one evidence and says how it ended, to w.
func storeFile(ctx context.Context, w io.Writer, db *badger.DB, path string, sopts StoreOptions) error {
	start := time.Now()
	logging.GetLogger().Info("StoreFile START", zap.String("file_path", path))
	fmt.Fprintf(w, "\nStoring %s\n", termsafe.Line(path))
	res, err := ingest.Run(ctx, db, ingest.Request{
		Path:            path,
		Decode:          sopts.Decode,
		NoIndex:         sopts.NoIndex,
		FTS:             sopts.FTS,
		Enrich:          sopts.Enrich,
		RequireVerified: sopts.RequireVerified,
		Quiet:           sopts.Quiet,
	})
	if res.Outcome != "" {
		reportIngest(w, res, time.Since(start))
	}
	logging.GetLogger().Info("StoreFile COMPLETE",
		zap.String("file_path", path),
		zap.String("outcome", string(res.Outcome)),
		zap.String("evidence_id", base64.StdEncoding.EncodeToString(res.EvidenceID)),
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.Error(err))
	var incomplete *ingest.IndexIncompleteError
	var mismatch *ingest.DigestMismatchError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &mismatch) && mismatch.Stored, errors.As(err, &incomplete):
		// Stored, but something needs the examiner's attention: exit 2.
		fmt.Fprintln(w, "Warning:", termsafe.Line(err.Error()))
		return &lifecycle.WarningError{Err: err}
	}
	return err
}

// reportIngest says how an ingest ended, to w.
func reportIngest(w io.Writer, res ingest.Result, took time.Duration) {
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
