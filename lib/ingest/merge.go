package ingest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"runtime/trace"
	"slices"

	"indicer/internal/progress"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/evidence"
	"indicer/lib/parser/core"
	"indicer/lib/parser/image"
	"indicer/lib/store"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// MergeRequest is one evidence of another database to merge into the
// session's.
type MergeRequest struct {
	// Source is the database the evidence is read from; it is only read.
	Source *badger.DB
	// SourceID is the evidence's raw ID in Source.
	SourceID []byte
	// Origin is the package the evidence came with: Package, the actor
	// fields, Trusted and At. The rest is filled from the source record.
	Origin structs.MergeInfo
	// Quiet shows no progress bar.
	Quiet bool
	// RequireVerified fails an EWF evidence whose stored digests do not
	// match the merged bytes, instead of storing it flagged.
	RequireVerified bool
	// MaxWorkers caps the chunk pass's workers; 0 leaves the configured
	// width.
	MaxWorkers int
	// Credit places the merge in a run of ingests (Request.Credit).
	Credit store.Credit
	// Cache is shared by the evidence's readers; nil gives each its own.
	Cache *dbio.ChunkCache
}

// ErrNotMergeable means a source evidence cannot be merged; the error wraps
// it with the reason.
var ErrNotMergeable = errors.New("evidence cannot be merged")

// ContentMismatchError means the merged bytes are not the content the source
// record names: Got is their hash in the recorded algorithm Algo, Recorded the
// hash the record holds. Nothing is committed.
type ContentMismatchError struct {
	SourceID []byte
	Algo     string
	Recorded string
	Got      string
}

func (e *ContentMismatchError) Error() string {
	return fmt.Sprintf("evidence %s does not hold the content its record names: its %s hash is %s, the record holds %s", b64(e.SourceID), e.Algo, e.Got, e.Recorded)
}

// Merge stores the evidence req.SourceID of req.Source in the session's
// database, in this database's layout: its stored chunks are read, checked
// against their hashes and against the content hash the source records,
// stored again, indexed here, and committed with where they came from
// (structs.MergeInfo). Content this database already holds becomes an
// alias at commit. Evidence merged before is CommitPresent, and nothing is
// written.
func (s *Session) Merge(ctx context.Context, req MergeRequest) (Result, error) {
	end, err := s.begin()
	if err != nil {
		return Result{}, err
	}
	defer end()
	if req.Quiet {
		ctx = progress.Without(ctx)
	}
	sid := dbio.EvidenceRawID(req.SourceID)
	srcRec, m, err := mergeSource(req, sid)
	if err != nil {
		return Result{}, err
	}
	target, present, _, err := s.presentTarget(ctx, req, sid, srcRec, m)
	switch {
	case err != nil:
		return Result{Size: srcRec.Size}, err
	case present:
		return Result{EvidenceID: target, Outcome: store.CommitPresent, Size: srcRec.Size}, nil
	}
	return s.merge(ctx, req, sid, srcRec, m)
}

// merge stores the source evidence sid, whose record is srcRec, as the
// merged evidence m.
func (s *Session) merge(ctx context.Context, req MergeRequest, sid []byte, srcRec structs.EvidenceFile, m structs.MergeInfo) (Result, error) {
	res := Result{Size: srcRec.Size}
	ec, err := dbio.OpenEvidenceChunks(req.Source, sid, dbio.ReadOptions{Cache: req.Cache})
	if err != nil {
		return res, err
	}
	img := storedImage{ec: ec, size: srcRec.Size}
	region := trace.StartRegion(ctx, "fingerprint")
	fp, err := evidence.Fingerprint(img, srcRec.Size, dbmeta.FileHasher(s.db))
	region.End()
	if err != nil {
		return res, fmt.Errorf("fingerprint evidence %s: %w", b64(sid), err)
	}

	info, skip, noIndex, err := mergedSource(req.Source, sid, srcRec)
	if err != nil {
		return res, err
	}
	res.Source = info
	revrel, err := store.HasReverseRelations(req.Source, sid, srcRec)
	if err != nil {
		return res, err
	}
	simhash, err := store.HasSignatures(req.Source, sid, srcRec)
	if err != nil {
		return res, err
	}
	second, check, err := expectOf(srcRec, m, req.Source, s.db, sid, img)
	if err != nil {
		return res, err
	}

	name := store.CleanName(srcRec.Name)
	aliasPath := name
	if len(info.Members) > 0 {
		aliasPath = info.Members[0].Path
	}
	in := input{
		img: img, size: srcRec.Size, parallel: true,
		info: info, ewf: ewfMetaOf(info.EWF), skip: skip,
		name: name, evidenceType: srcRec.EvidenceType, fingerprint: fp, aliasPath: aliasPath,
		attemptKey: dbio.MergeAttemptKey(m, srcRec.Size), preferredID: sid, merged: &m,
		secondHash: second, check: check,
	}
	r := Request{
		NoIndex: noIndex, FTS: hasFeature(srcRec, store.FeatureFTS), Enrich: hasFeature(srcRec, store.FeatureEnrich),
		RevRel: revrel, Simhash: simhash, RequireVerified: req.RequireVerified, Quiet: req.Quiet,
		MaxWorkers: req.MaxWorkers, Credit: req.Credit,
	}
	return s.ingestPrepared(ctx, in, r, res)
}

// MergePresent finishes the merge of an evidence the session's database
// already holds: it carries the source's aliases onto the evidence holding
// the content. Content another package merged is read and checked against
// the hash this source records first, so a package cannot attach its
// aliases to content it does not hold. An evidence that is not held is
// merged.
func (s *Session) MergePresent(ctx context.Context, req MergeRequest) (Result, error) {
	end, err := s.begin()
	if err != nil {
		return Result{}, err
	}
	defer end()
	if req.Quiet {
		ctx = progress.Without(ctx)
	}
	sid := dbio.EvidenceRawID(req.SourceID)
	srcRec, m, err := mergeSource(req, sid)
	if err != nil {
		return Result{}, err
	}
	res := Result{Size: srcRec.Size}
	target, present, verify, err := s.presentTarget(ctx, req, sid, srcRec, m)
	if err != nil {
		return res, err
	}
	if !present {
		return s.merge(ctx, req, sid, srcRec, m)
	}
	res.EvidenceID, res.Outcome = target, store.CommitPresent
	if verify {
		if err := verifySource(ctx, req, sid, srcRec, m); err != nil {
			return res, err
		}
	}
	aliases, err := dbio.Aliases(req.Source, sid)
	if err != nil {
		return res, fmt.Errorf("aliases of evidence %s: %w", b64(sid), err)
	}
	if _, err := store.CarryAliases(s.db, target, m, srcRec.Size, aliases); err != nil {
		return res, err
	}
	return res, nil
}

// mergeSource reads the source evidence sid and the MergeInfo its merge
// records, or why it cannot be merged.
func mergeSource(req MergeRequest, sid []byte) (structs.EvidenceFile, structs.MergeInfo, error) {
	srcRec, err := dbio.GetEvidenceFile(sid, req.Source)
	if err != nil {
		return srcRec, structs.MergeInfo{}, fmt.Errorf("source evidence %s: %w", b64(sid), err)
	}
	if err := mergeable(srcRec); err != nil {
		return srcRec, structs.MergeInfo{}, fmt.Errorf("evidence %s: %w", b64(sid), err)
	}
	m := req.Origin
	m.SourceEvidenceID = append([]byte(nil), sid...)
	m.SourceFileHash = srcRec.FileHash
	m.SourceHashAlgo = srcRec.FileHashAlgo
	if m.SourceHashAlgo == "" {
		_, m.SourceHashAlgo = hashNames(req.Source)
	}
	m.Prior = nil
	if srcRec.Merged != nil {
		prior := *srcRec.Merged
		prior.Prior = nil
		m.Prior = &prior
	}
	return srcRec, m, nil
}

// mergeable says why e cannot be merged, or nil.
func mergeable(e structs.EvidenceFile) error {
	switch {
	case e.IngestState == structs.IngestStateDuplicate:
		return fmt.Errorf("%w: it is a duplicate of evidence %s", ErrNotMergeable, b64(e.DuplicateOf))
	case e.IsIngestIncomplete():
		return fmt.Errorf("%w: it is not committed in the source (%s); run `dues repair --fix` on the source database with write access and export it again", ErrNotMergeable, e.IngestState)
	case e.Start != 0:
		return fmt.Errorf("%w: it starts at offset %d", ErrNotMergeable, e.Start)
	case e.FileHash == "":
		return fmt.Errorf("%w: it records no content hash", ErrNotMergeable)
	case e.ContentModel == structs.ContentModelV2 && !fileHashNamesContent(e):
		return fmt.Errorf("%w: its file hash is neither its plain hash nor, under the %s strategy, its content root", ErrNotMergeable, cnst.HochoHashStrategy)
	}
	return nil
}

// fileHashNamesContent says whether the FileHash of e, which keys the merge
// ledger, is one of the hashes the merge checks the bytes against, as an
// ingest records it.
func fileHashNamesContent(e structs.EvidenceFile) bool {
	if e.FileHash == e.PlainHash {
		return true
	}
	return e.HashStrategy == cnst.HochoHashStrategy && e.ContentRoot != "" && e.FileHash == e.ContentRoot
}

// presentTarget returns the evidence of the session's database that already
// holds the merged evidence, and whether its content must be read before the
// source's aliases are carried onto it: what another package merged vouches
// for that package's bytes, not for this source's.
func (s *Session) presentTarget(ctx context.Context, req MergeRequest, sid []byte, srcRec structs.EvidenceFile, m structs.MergeInfo) (target []byte, present, verify bool, err error) {
	entry, present, err := store.MergedTarget(s.db, m, srcRec.Size)
	if err != nil || present {
		return entry.Target, present, entry.Package != m.Package, err
	}
	from, shared, err := store.SharedIDPresent(ctx, s.db, req.Source, sid, srcRec)
	if err != nil || !shared {
		return nil, false, false, err
	}
	return sid, true, from != nil && from.Package != m.Package, nil
}

// verifySource reads the source evidence and checks its bytes against the
// content hash its record holds, in the source's own layout.
func verifySource(ctx context.Context, req MergeRequest, sid []byte, srcRec structs.EvidenceFile, m structs.MergeInfo) error {
	ec, err := dbio.OpenEvidenceChunks(req.Source, sid, dbio.ReadOptions{Cache: req.Cache})
	if err != nil {
		return err
	}
	sums, err := store.HashContentChunked(ctx, req.Source, storedImage{ec: ec, size: srcRec.Size}, srcRec.Size, ec.ChunkSize(), true)
	if err != nil {
		return fmt.Errorf("hash evidence %s: %w", b64(sid), err)
	}
	enc := base64.StdEncoding.EncodeToString
	plain, root := enc(sums.Plain), enc(sums.Root)
	if srcRec.ContentModel == structs.ContentModelV2 {
		switch {
		case plain != srcRec.PlainHash:
			return mismatch(sid, m.SourceHashAlgo, srcRec.PlainHash, plain)
		case srcRec.ContentRoot != "" && root != srcRec.ContentRoot:
			return mismatch(sid, m.SourceHashAlgo, srcRec.ContentRoot, root)
		case srcRec.FileHash != plain && srcRec.FileHash != root:
			return mismatch(sid, m.SourceHashAlgo, srcRec.FileHash, plain)
		}
		return nil
	}
	if plain != srcRec.FileHash && root != srcRec.FileHash {
		return mismatch(sid, m.SourceHashAlgo, srcRec.FileHash, plain)
	}
	return nil
}

func mismatch(sid []byte, algo, recorded, got string) error {
	return &ContentMismatchError{SourceID: append([]byte(nil), sid...), Algo: algo, Recorded: recorded, Got: got}
}

// mergedSource is the source description the merged evidence records, why
// it is not indexed (""), and whether the source was stored without an index
// on request. The source's index state is never carried: the skip follows
// from the content, and evidence stored before the content model as the raw
// bytes of a container records that container here.
func mergedSource(db *badger.DB, sid []byte, e structs.EvidenceFile) (structs.EvidenceSource, string, bool, error) {
	info := copySource(e.Source)
	if e.ContentModel != structs.ContentModelV2 && e.Size > 0 {
		format, err := store.StoredContainerFormat(db, sid)
		if err != nil {
			return info, "", false, fmt.Errorf("evidence %s: %w", b64(sid), err)
		}
		if format != core.FormatRaw {
			info.Format, info.Container = structs.SourceRaw, string(format)
			return info, evidence.SkipNotDecoded, false, nil
		}
	}
	switch {
	case info.Format == structs.SourceOpaque:
		return info, evidence.SkipUnsupportedContainer, false, nil
	case info.Format == structs.SourceRaw && info.Container != "":
		return info, evidence.SkipNotDecoded, false, nil
	case e.Size == 0:
		return info, evidence.SkipEmpty, false, nil
	}
	notRequested := e.Index != nil && e.Index.State == structs.IndexSkipped && e.Index.Reason == evidence.SkipNotRequested
	return info, "", notRequested, nil
}

// copySource is a deep copy of src, or a raw source of no members for a
// record that has none.
func copySource(src *structs.EvidenceSource) structs.EvidenceSource {
	if src == nil {
		return structs.EvidenceSource{Format: structs.SourceRaw}
	}
	info := *src
	info.Members = slices.Clone(src.Members)
	if src.EWF != nil {
		c := *src.EWF
		c.StoredMD5, c.StoredSHA1 = slices.Clone(c.StoredMD5), slices.Clone(c.StoredSHA1)
		c.MD5, c.SHA1 = slices.Clone(c.MD5), slices.Clone(c.SHA1)
		c.AcquisitionErrors = slices.Clone(c.AcquisitionErrors)
		info.EWF = &c
	}
	return info
}

// ewfMetaOf is what the chunk pass verifies the merged bytes against: the
// digests the EWF set stored when it was first ingested, or nil.
func ewfMetaOf(c *structs.EWFCheck) *image.EWFMeta {
	if c == nil {
		return nil
	}
	m := &image.EWFMeta{MD5: c.StoredMD5, SHA1: c.StoredSHA1, ChunkTablesInvalid: c.ChunkTablesInvalid}
	for _, r := range c.AcquisitionErrors {
		m.AcquisitionErrors = append(m.AcquisitionErrors, image.ByteRange{Offset: r.Offset, Length: r.Length})
	}
	return m
}

// expectOf builds the check that the merged bytes are the content the
// source record srcRec names, and the second hash it needs when the source
// hashed content with another algorithm than the target. A chunk that
// matches its own hash is not enough: which chunk sits at which offset comes
// from the source's relations.
func expectOf(srcRec structs.EvidenceFile, m structs.MergeInfo, source, target *badger.DB, sid []byte, img storedImage) (func() hash.Hash, func(context.Context, store.ChunkResult) error, error) {
	targetChunk, targetFile := hashNames(target)
	var second func() hash.Hash
	if m.SourceHashAlgo != targetFile {
		h, err := dbmeta.FileHasherNamed(m.SourceHashAlgo)
		if err != nil {
			return nil, nil, fmt.Errorf("evidence %s: %w", b64(sid), err)
		}
		second = h
	}
	sourceChunk, _ := hashNames(source)
	if srcRec.ChunkHashAlgo != "" {
		sourceChunk = srcRec.ChunkHashAlgo
	}
	sameLayout := second == nil && sourceChunk == targetChunk && img.ec.ChunkSize() == dbmeta.ChunkSize(target)
	enc := base64.StdEncoding.EncodeToString
	sourceRoot := func(ctx context.Context, chunks store.ChunkResult) (string, error) {
		if sameLayout {
			return enc(chunks.Root), nil
		}
		sums, err := store.HashContentChunked(ctx, source, img, srcRec.Size, img.ec.ChunkSize(), true)
		if err != nil {
			return "", fmt.Errorf("hash evidence %s in its own layout: %w", b64(sid), err)
		}
		return enc(sums.Root), nil
	}
	check := func(ctx context.Context, chunks store.ChunkResult) error {
		plainSum := chunks.PlainHash
		if second != nil {
			plainSum = chunks.SecondPlain
		}
		plain := enc(plainSum)
		if srcRec.ContentModel == structs.ContentModelV2 {
			if plain != srcRec.PlainHash {
				return mismatch(sid, m.SourceHashAlgo, srcRec.PlainHash, plain)
			}
			// A hocho FileHash keys the merge ledger, so its root is checked in
			// every layout; a plain one only where the pass computed the root.
			if srcRec.ContentRoot == "" || (!sameLayout && srcRec.FileHash == srcRec.PlainHash) {
				return nil
			}
			root, err := sourceRoot(ctx, chunks)
			if err != nil {
				return err
			}
			if root != srcRec.ContentRoot {
				return mismatch(sid, m.SourceHashAlgo, srcRec.ContentRoot, root)
			}
			return nil
		}
		if plain == srcRec.FileHash {
			return nil
		}
		// A legacy hocho ingest recorded the content root as its FileHash.
		root, err := sourceRoot(ctx, chunks)
		if err != nil {
			return err
		}
		if root != srcRec.FileHash {
			return mismatch(sid, m.SourceHashAlgo, srcRec.FileHash, plain)
		}
		return nil
	}
	return second, check, nil
}
