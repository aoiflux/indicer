// Package ingest stores one evidence (W6): it opens the source decoded (D5),
// finds content already stored (D1), stores the chunks and indexes the
// evidence side by side, commits the evidence's identity, and then runs the
// full-text and graph features over its index.
//
// One ingest has exactly one outcome: stored, aliased to evidence that
// already holds the content, or failed (I6). The chunk pass and the index pass
// run under separate cancellation: an index failure never stops storage, and a
// storage failure stops indexing. Nothing Run starts outlives it.
package ingest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"runtime/trace"
	"sync"
	"time"

	"indicer/internal/faultinject"
	"indicer/internal/progress"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/engine"
	"indicer/lib/evidence"
	"indicer/lib/index"
	"indicer/lib/parser/image"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// Request is one ingest.
type Request struct {
	// Path is the file to store: a raw image, the first segment of an EWF or
	// split raw set, a VHD/VHDX, or any other file.
	Path string
	// Name is the evidence's display name; the file's base name when empty.
	Name string
	// SourcePath, when set, is recorded as the path the evidence was read
	// from instead of Path: an upload records the path its client gave, not
	// the temporary file it was received into. It applies to a source of
	// one file.
	SourcePath string
	// Decode stores a recognised container decoded (D5). Without it the
	// file's own bytes are stored and not indexed: the compression product
	// archives files, and must give back exactly the file it was given.
	Decode bool
	// NoIndex stores without indexing.
	NoIndex bool
	// FTS and Enrich run the full-text and graph features after the commit.
	FTS, Enrich bool
	// RevRel writes each chunk's reverse relation, which near needs; Simhash
	// writes each chunk's SimHash signature, which near's deep mode compares.
	RevRel, Simhash bool
	// RequireVerified fails an EWF ingest whose stored digests do not match
	// the decoded image, instead of storing it flagged.
	RequireVerified bool
	// Quiet shows no progress bar.
	Quiet bool
	// MaxWorkers caps the chunk pass's workers, and those of each object the
	// index pass stores; 0 leaves the configured width (store.ChunkPass).
	MaxWorkers int
	// Credit places the ingest in a run of ingests that count the chunks
	// they share as one at a time, in order, would (store.ChunkCredits): the
	// evidence of a folder store.
	Credit store.Credit
}

// Result is how an ingest ended.
type Result struct {
	// EvidenceID is the raw ID of the evidence holding the content: this
	// ingest's own, or, for an alias, the evidence that already held it.
	EvidenceID []byte
	Outcome    store.CommitOutcome
	// Attempt is the raw ID this ingest stored under; after an alias its
	// records are gone and only the alias names it. Empty when the content
	// was found stored before anything was written.
	Attempt []byte
	// Resumed: an earlier, unfinished ingest of the same source carried on.
	Resumed bool
	Source  structs.EvidenceSource
	Size    int64
	// Index is the index state recorded on the evidence.
	Index *structs.IndexInfo
	// Partitions are the partition records the index pass wrote.
	Partitions []index.Partition
	// Bytes counts the chunk pass; zero when nothing was stored.
	Bytes Bytes
}

// Bytes counts an ingest's chunk pass: the bytes it read, those new to the
// database after deduplication, and what the new ones take on disk,
// compressed and, in an encrypted database, sealed. Decoded content the
// index pass stores is not counted. In a run (Request.Credit), New and
// Written are what the evidence's pass stored first;
// store.ChunkCredits.Adjust says what to add for a pass at a time.
type Bytes struct {
	Read, New, Written int64
}

// Add adds o to b.
func (b *Bytes) Add(o Bytes) {
	b.Read += o.Read
	b.New += o.New
	b.Written += o.Written
}

// IndexIncompleteError means the evidence was stored and committed, but its
// index is not complete: the pass failed, a feature run after it failed, or
// some of it could not be indexed. The CLI exits 2 (lifecycle warning).
type IndexIncompleteError struct {
	EvidenceID []byte
	State      structs.IndexState
	Reason     string
	Err        error
}

func (e *IndexIncompleteError) Error() string {
	msg := fmt.Sprintf("evidence %s stored, but its index is %s", base64.StdEncoding.EncodeToString(e.EvidenceID), e.State)
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	return msg
}

func (e *IndexIncompleteError) Unwrap() error { return e.Err }

// ErrDigestMismatch means an EWF set's stored MD5 or SHA-1 does not match
// the image it decodes to.
var ErrDigestMismatch = errors.New("the decoded image does not match the digests its EWF set stores")

// DigestMismatchError reports an EWF ingest whose digests did not match. The
// evidence is stored and flagged unless RequireVerified was asked for.
type DigestMismatchError struct {
	EvidenceID []byte
	Stored     bool
	Check      structs.EWFCheck
}

func (e *DigestMismatchError) Error() string {
	if e.Stored {
		return fmt.Sprintf("evidence %s stored, but %v", base64.StdEncoding.EncodeToString(e.EvidenceID), ErrDigestMismatch)
	}
	return ErrDigestMismatch.Error()
}

func (e *DigestMismatchError) Unwrap() error { return ErrDigestMismatch }

// passes are the passes an ingest runs. A session runs these; a test gives
// its own to the session it makes.
type passes struct {
	index  func(context.Context, index.Options) (index.Result, error)
	chunks func(context.Context, store.ChunkPass) (store.ChunkResult, error)
}

func defaultPasses() passes { return passes{index: index.Run, chunks: store.IngestChunks} }

// Run ingests req.Path into db, in a session of its own: the full-text index
// and graph it opens are closed before it returns.
func Run(ctx context.Context, db *badger.DB, req Request) (Result, error) {
	s := NewSession(db)
	res, err := s.Run(ctx, req)
	return res, s.closeWith(err)
}

// Run ingests req.Path into the session's database.
func (s *Session) Run(ctx context.Context, req Request) (Result, error) {
	end, err := s.begin()
	if err != nil {
		return Result{}, err
	}
	defer end()
	db := s.db
	// One strategy for the whole ingest: its plan, record and lookup agree.
	strategy := engine.For(db).Tuning().HashStrategy
	if req.Quiet {
		ctx = progress.Without(ctx)
	}
	// Under --trace, each step of an ingest is a region of the caller's task.
	region := trace.StartRegion(ctx, "open")
	src, err := evidence.Open(ctx, req.Path, evidence.Options{Decode: req.Decode})
	region.End()
	if err != nil {
		return Result{}, err
	}
	defer src.Close()

	res := Result{Source: recordedSource(src, req), Size: src.Size()}
	name := req.Name
	if name == "" {
		name = src.Name()
	}
	region = trace.StartRegion(ctx, "fingerprint")
	fp, err := src.Fingerprint(dbmeta.FileHasher(db))
	region.End()
	if err != nil {
		return res, fmt.Errorf("fingerprint %s: %w", req.Path, err)
	}
	rec, err := newRecord(db, src, res.Source, name, fp, strategy, req)
	if err != nil {
		return res, err
	}

	planned := planIngest(ctx, src, strategy, req)
	// D1: content already stored is an alias, and nothing is stored again.
	winner, err := findStored(ctx, db, src, fp, strategy)
	if err != nil {
		return res, err
	}
	if winner != nil {
		progress.Unplan(ctx, planned...)
		path := src.Path()
		if req.SourcePath != "" {
			path = req.SourcePath
		}
		a := structs.AliasRecord{Name: name, Path: path, Size: src.Size(), Source: res.Source}
		if err := store.WriteAlias(db, winner, a); err != nil {
			return res, err
		}
		res.EvidenceID, res.Outcome = winner, store.CommitAliased
		return res, nil
	}

	region = trace.StartRegion(ctx, "begin")
	eid, resumed, err := store.BeginAttempt(db, attemptPath(res.Source, src), rec)
	region.End()
	if err != nil {
		return res, err
	}
	res.Attempt, res.Resumed = eid, resumed
	if err := faultinject.Hit(faultinject.IngestAfterPending); err != nil {
		return res, fail(db, eid, err)
	}
	return s.run(ctx, src, eid, name, req, res)
}

// planIngest lists the steps an ingest of src with the hash strategy
// strategy will run, with the work each expects, before the first starts, so
// the overall measure weighs them from the beginning. It returns their names.
func planIngest(ctx context.Context, src *evidence.Source, strategy string, req Request) []string {
	size := src.Size()
	var steps []progress.Step
	if strategy == cnst.SyncHashStrategy {
		steps = append(steps, progress.Step{Name: "identify", Unit: progress.Bytes, Total: size})
	}
	steps = append(steps,
		progress.Step{Name: "store", Unit: progress.Bytes, Total: size},
		progress.Step{Name: "index", Unit: progress.Files, Total: progress.Unknown})
	if indexSkipReason(src, req) == "" {
		steps = append(steps, featureSteps(req, size)...)
	}
	progress.PlanSteps(ctx, steps...)
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.Name
	}
	return names
}

// featureSteps are the steps the features of req add after the index;
// entropy reads about size bytes.
func featureSteps(req Request, size int64) []progress.Step {
	var steps []progress.Step
	if req.FTS {
		steps = append(steps, progress.Step{Name: "full-text", Unit: progress.Files, Total: progress.Unknown})
	}
	if req.Enrich {
		steps = append(steps,
			progress.Step{Name: "entropy", Unit: progress.Bytes, Total: size},
			progress.Step{Name: "graph", Unit: progress.Files, Total: progress.Unknown})
	}
	return steps
}

// skipFeatures shows the planned features as not run, and why: the index
// they read did not finish.
func skipFeatures(ctx context.Context, req Request, why string) {
	for _, s := range featureSteps(req, 0) {
		progress.Skip(ctx, s.Name, why)
	}
}

// attemptPath is the path an attempt is keyed by: the first source member the
// record keeps, which is what the commit clears the key by, else the path
// read.
func attemptPath(info structs.EvidenceSource, src *evidence.Source) string {
	if len(info.Members) > 0 {
		return info.Members[0].Path
	}
	return src.Path()
}

// recordedSource is what the evidence records about where it was read from.
func recordedSource(src *evidence.Source, req Request) structs.EvidenceSource {
	info := src.Info()
	if req.SourcePath != "" && len(info.Members) == 1 {
		info.Members = []structs.SourceMember{{Path: req.SourcePath, Size: info.Members[0].Size}}
	}
	return info
}

// newRecord builds the evidence record an ingest with the hash strategy
// strategy starts from.
func newRecord(db *badger.DB, src *evidence.Source, info structs.EvidenceSource, name, fp, strategy string, req Request) (structs.EvidenceFile, error) {
	switch strategy {
	case cnst.SyncHashStrategy, cnst.AsyncHashStrategy, cnst.HochoHashStrategy:
	default:
		return structs.EvidenceFile{}, fmt.Errorf("unsupported store hash strategy: %s", strategy)
	}
	head := make([]byte, 4096)
	n, err := src.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return structs.EvidenceFile{}, fmt.Errorf("read %s: %w", src.Path(), err)
	}
	rec := structs.NewEvidenceFile(name, 0, src.Size(), nil, util.DetectEvidenceType(name, head[:n]), "")
	rec.ContentModel = structs.ContentModelV2
	rec.ChunkSize = dbmeta.ChunkSize(db)
	rec.ChunkHashAlgo, rec.FileHashAlgo = hashNames(db)
	rec.HashStrategy = strategy
	rec.Fingerprint = fp
	rec.Source = &info
	if req.RevRel {
		rec.Features = append(rec.Features, store.FeatureRevrel)
	}
	if req.Simhash {
		rec.Features = append(rec.Features, store.FeatureSimhash)
	}
	if req.FTS {
		rec.Features = append(rec.Features, "fts")
	}
	if req.Enrich {
		rec.Features = append(rec.Features, "enrich")
	}
	return rec, nil
}

func hashNames(db *badger.DB) (chunk, file string) {
	if dbmeta.HashAlgo(db) == dbmeta.HashSHA3 {
		return "sha3-512", "sha3-256"
	}
	return "blake3", "blake3"
}

// findStored returns the COMPLETED evidence that already holds src's
// content, or nil. The sync strategy hashes the content first and looks the
// hashes up. The others look up evidence of the same size and fingerprint,
// which identifies small content exactly and is confirmed for larger content
// by computing its root.
func findStored(ctx context.Context, db *badger.DB, src *evidence.Source, fp, strategy string) ([]byte, error) {
	defer trace.StartRegion(ctx, "identify").End()
	if strategy == cnst.SyncHashStrategy {
		sums, err := identify(ctx, db, src)
		if err != nil {
			return nil, err
		}
		enc := base64.StdEncoding.EncodeToString
		return store.StoredByIdentity(db, src.Size(), enc(sums.Root), enc(sums.Plain))
	}
	cands, err := store.QuickCandidates(db, src.Size(), fp)
	if err != nil || len(cands) == 0 {
		return nil, err
	}
	if src.Size() <= evidence.FingerprintFullLimit {
		return cands[0].ID, nil
	}
	var root string
	for _, c := range cands {
		if c.Root == "" {
			continue
		}
		if root == "" {
			sums, err := identify(ctx, db, src)
			if err != nil {
				return nil, err
			}
			root = base64.StdEncoding.EncodeToString(sums.Root)
		}
		if c.Root == root {
			return c.ID, nil
		}
	}
	return nil, nil
}

// identifyCost is what a byte identify hashes weighs on the overall bar
// beside a byte store stores. Identify only reads and hashes; store also
// chunks, compresses and writes. Measured: a 10 GiB E01 identified 8 times
// faster than it stored (921 against 113 MiB/s), a file in the page cache 18
// times; from a disk slower than store, both run at the disk's speed. A
// quarter sits between, so the bar neither jumps to half when identify ends
// nor crawls through it.
const identifyCost = 0.25

// identify hashes src's content to find it among the stored evidence.
func identify(ctx context.Context, db *badger.DB, src *evidence.Source) (store.ContentSums, error) {
	ph := progress.Start(ctx, "identify", progress.Bytes, src.Size())
	ph.Cost(identifyCost)
	sums, err := store.HashContent(progress.Within(ctx, ph), db, src, src.Size(), src.Parallel())
	ph.Done(err)
	return sums, err
}

// run stores and indexes the attempt eid, then commits it.
func (s *Session) run(ctx context.Context, src *evidence.Source, eid []byte, name string, req Request, res Result) (Result, error) {
	db := s.db
	reason := indexSkipReason(src, req)
	indexing := reason == ""
	info := &structs.IndexInfo{State: structs.IndexSkipped, Reason: reason}
	if indexing {
		info = &structs.IndexInfo{State: structs.IndexRunning, Started: time.Now().UnixNano()}
	}
	if err := setIndexInfo(db, eid, info); err != nil {
		return res, err
	}
	if !indexing {
		progress.Skip(ctx, "index", reason)
	}

	// The index pass gets its own cancellation: storage failing stops it,
	// and it failing never stops storage.
	idxCtx, stopIndex := context.WithCancelCause(ctx)
	defer stopIndex(nil)
	var ranges []store.ByteRange
	var idx index.Result
	var idxErr error
	var wg sync.WaitGroup
	if indexing {
		ranges = index.PartitionRanges(src.Image())
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer trace.StartRegion(idxCtx, "index").End()
			idx, idxErr = s.passes.index(idxCtx, index.Options{DB: db, EvidenceID: eid, EvidenceName: name, Image: src.Image(), Entropy: req.Enrich, RevRel: req.RevRel, Simhash: req.Simhash, MaxWorkers: req.MaxWorkers, Credit: req.Credit})
		}()
	}

	region := trace.StartRegion(ctx, "chunks")
	chunks, err := s.passes.chunks(ctx, store.ChunkPass{
		DB: db, ID: eid, Source: src.Image(), Size: src.Size(),
		Parallel: src.Parallel(), Ranges: ranges, EWF: src.EWF(), Quiet: req.Quiet,
		RevRel: req.RevRel, Simhash: req.Simhash, MaxWorkers: req.MaxWorkers,
		Credit: req.Credit,
	})
	region.End()
	if err != nil {
		stopIndex(fmt.Errorf("storage failed: %w", err))
		wg.Wait()
		return res, fail(db, eid, err)
	}

	res.Bytes = Bytes{Read: src.Size(), New: chunks.NewBytes, Written: chunks.WrittenBytes}
	check := ewfCheck(res.Source.EWF, chunks.EWF)
	if check != nil {
		res.Source.EWF = check
	}
	if check != nil && check.Result == structs.EWFMismatch && req.RequireVerified {
		stopIndex(ErrDigestMismatch)
		wg.Wait()
		mis := &DigestMismatchError{EvidenceID: eid, Check: *check}
		return res, fail(db, eid, mis)
	}
	if err := faultinject.Hit(faultinject.IngestBeforeFlushed); err != nil {
		stopIndex(err)
		wg.Wait()
		return res, fail(db, eid, err)
	}
	region = trace.StartRegion(ctx, "flush")
	err = flush(db, eid, chunks, check)
	region.End()
	if err != nil {
		stopIndex(err)
		wg.Wait()
		return res, err
	}
	// What is left of the index pass, which ran beside the chunks.
	region = trace.StartRegion(ctx, "index-wait")
	wg.Wait()
	region.End()

	// A failure here leaves the attempt FLUSHED, as a crash would: recovery
	// commits it.
	if err := faultinject.Hit(faultinject.IngestBeforeCommit); err != nil {
		return res, err
	}
	region = trace.StartRegion(ctx, "commit")
	commit, err := store.CommitEvidence(db, eid)
	region.End()
	if err != nil {
		return res, err
	}
	if commit.Outcome == store.CommitAliased {
		// Another ingest of the same content committed first.
		res.EvidenceID, res.Outcome = commit.Winner, store.CommitAliased
		if err := store.CleanupDuplicate(ctx, db, eid); err != nil {
			return res, fmt.Errorf("remove the records of duplicate attempt %x: %w", eid, err)
		}
		return res, nil
	}
	res.EvidenceID, res.Outcome = eid, store.CommitStored

	var incomplete *IndexIncompleteError
	if indexing {
		res.Partitions = idx.Partitions
		info.Coverage = idx.Coverage
		info.Finished = time.Now().UnixNano()
		switch {
		case idxErr != nil:
			info.State, info.Reason = structs.IndexFailed, idxErr.Error()
		default:
			info.State, info.Reason = idx.State, idx.Reason
			if err := setPartitionHashes(ctx, db, src, chunks, idx.Partitions); err != nil {
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
		if info.State != structs.IndexComplete {
			incomplete = &IndexIncompleteError{EvidenceID: eid, State: info.State, Reason: info.Reason, Err: idxErr}
		}
	}
	if err := setIndexInfo(db, eid, info); err != nil {
		return res, err
	}
	res.Index = info
	if check != nil && check.Result == structs.EWFMismatch {
		return res, errors.Join(&DigestMismatchError{EvidenceID: eid, Stored: true, Check: *check}, nilIfNone(incomplete))
	}
	if incomplete != nil {
		return res, incomplete
	}
	return res, nil
}

func nilIfNone(e *IndexIncompleteError) error {
	if e == nil {
		return nil
	}
	return e
}

func indexSkipReason(src *evidence.Source, req Request) string {
	switch {
	case src.SkipIndex() != "":
		return src.SkipIndex()
	case req.NoIndex:
		return "not_requested"
	}
	return ""
}

// ewfCheck is what the chunk pass found about an EWF set's stored digests.
func ewfCheck(stored *structs.EWFCheck, got *image.DigestResult) *structs.EWFCheck {
	if stored == nil {
		return nil
	}
	c := *stored
	c.Result = structs.EWFUnverified
	if got != nil {
		c.MD5, c.SHA1 = got.MD5, got.SHA1
		switch {
		case got.OK():
			c.Result = structs.EWFVerified
		case got.MD5Checked || got.SHA1Checked:
			c.Result = structs.EWFMismatch
		}
	}
	return &c
}

// flush records what the chunk pass computed over exactly the bytes it
// stored, and marks the evidence FLUSHED: every chunk is durable (I2).
func flush(db *badger.DB, eid []byte, chunks store.ChunkResult, check *structs.EWFCheck) error {
	enc := base64.StdEncoding.EncodeToString
	return dbio.UpdateEvidence(db, eid, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		e.PlainHash, e.ContentRoot = enc(chunks.PlainHash), enc(chunks.Root)
		e.FileHash = e.PlainHash
		if e.HashStrategy == cnst.HochoHashStrategy {
			e.FileHash = e.ContentRoot
		}
		if check != nil && e.Source != nil {
			e.Source.EWF = check
		}
		e.IngestState, e.Failed, e.LastError = structs.IngestStateFlushed, false, ""
		return nil
	})
}

// fail marks the attempt failed with err, keeping it resumable, and returns
// err.
func fail(db *badger.DB, eid []byte, err error) error {
	merr := dbio.UpdateEvidence(db, eid, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		e.Failed, e.LastError = true, err.Error()
		if e.Index != nil && e.Index.State == structs.IndexRunning {
			e.Index.State, e.Index.Reason, e.Index.Finished = structs.IndexFailed, "storage failed", time.Now().UnixNano()
		}
		return nil
	})
	if merr != nil {
		return errors.Join(err, fmt.Errorf("mark evidence %x failed: %w", eid, merr))
	}
	return err
}

func setIndexInfo(db *badger.DB, eid []byte, info *structs.IndexInfo) error {
	return dbio.UpdateEvidence(db, eid, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		cp := *info
		e.Index = &cp
		return nil
	})
}

// setPartitionHashes records each partition's plain hash: from the chunk
// pass's range digests, the evidence's own hash for a filesystem spanning
// the whole device, and otherwise by reading the partition's range.
func setPartitionHashes(ctx context.Context, db *badger.DB, src *evidence.Source, chunks store.ChunkResult, parts []index.Partition) (err error) {
	defer trace.StartRegion(ctx, "partition-hashes").End()
	digests := map[store.ByteRange][]byte{{Offset: 0, Length: src.Size()}: chunks.PlainHash}
	for _, r := range chunks.Ranges {
		digests[r.ByteRange] = r.Hash
	}
	usable := func(p index.Partition) bool { return p.Size > 0 && p.Offset >= 0 && p.Size <= src.Size()-p.Offset }
	var need int64
	for _, p := range parts {
		if _, ok := digests[store.ByteRange{Offset: p.Offset, Length: p.Size}]; usable(p) && !ok {
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
		h, ok := digests[store.ByteRange{Offset: p.Offset, Length: p.Size}]
		if !ok {
			sums, err := store.HashContent(progress.Within(ctx, ph), db, io.NewSectionReader(src, p.Offset, p.Size), p.Size, src.Parallel())
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
