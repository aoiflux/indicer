package enrichment

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"indicer/internal/console"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/entropy"
	"indicer/lib/structs"
	"indicer/lib/util"
	"path"
	"strings"

	"github.com/dgraph-io/badger/v4"
	"github.com/schollz/progressbar/v3"
	"github.com/vmihailenco/msgpack/v5"
)

type Service struct {
	db         *badger.DB
	repository Repository
}

func NewService(db *badger.DB, repository Repository) *Service {
	return &Service{db: db, repository: repository}
}

func (service *Service) Close() error {
	if service == nil || service.repository == nil {
		return nil
	}
	return service.repository.Close()
}

func (service *Service) EnrichEvidence(record EvidenceRecord) error {
	if service == nil || service.repository == nil {
		return nil
	}
	return service.repository.UpsertEvidence(record)
}

// Report counts what an enrichment run wrote, and what it could not compute.
type Report struct {
	Evidence   int
	Partitions int
	Files      int
	// Quarantined is how many evidence have an index known to be wrong
	// (repair --upgrade): only the evidence itself is written for them.
	Quarantined int
	// Unreadable is how many objects' entropy could not be computed because
	// their content could not be read. Problems names the first of them.
	Unreadable int
	Problems   []string
}

const maxReportProblems = 10

func (r *Report) unreadable(n int, what string, err error) {
	r.Unreadable += n
	if len(r.Problems) < maxReportProblems {
		r.Problems = append(r.Problems, fmt.Sprintf("%s: %v", what, err))
	}
}

func (r Report) err() error {
	if r.Unreadable > 0 {
		return &IncompleteError{Report: r}
	}
	return nil
}

// IncompleteError reports an enrichment that wrote the graph but could not
// read some content to compute its entropy. The nodes are written without
// entropy. It used to be shown as "not computed", which looked the same as
// enrichment never having been asked for it (GRA-19).
type IncompleteError struct{ Report Report }

func (e *IncompleteError) Error() string {
	msg := fmt.Sprintf("graph enrichment: the entropy of %d object(s) could not be computed, because their content could not be read", e.Report.Unreadable)
	if len(e.Report.Problems) > 0 {
		msg += " (first: " + e.Report.Problems[0] + ")"
	}
	return msg
}

type evidenceContext struct {
	evidenceFileID   string
	evidenceFileName string
}

type partitionContext struct {
	evidenceContext
	partitionID   string
	partitionName string
}

// EnrichAll upserts the hierarchy of every completed evidence into graphdb:
// evidence file -> partition -> indexed file nodes. Each evidence is read in
// transactions of its own, not under one held for the whole run.
func (service *Service) EnrichAll(ctx context.Context) (Report, error) {
	var rep Report
	if service == nil || service.db == nil || service.repository == nil {
		return rep, nil
	}

	var ids [][]byte
	var totalWork int64
	err := service.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 100
		it := txn.NewIterator(opts)
		defer it.Close()

		prefix := []byte(cnst.EviFileNamespace)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			value, err := it.Item().ValueCopy(nil)
			if err != nil {
				return err
			}
			var evidata structs.EvidenceFile
			if err := msgpack.Unmarshal(decodeStoredValue(value), &evidata); err != nil {
				return err
			}
			if !evidata.Completed || evidata.IngestState == structs.IngestStateDuplicate {
				continue
			}
			ids = append(ids, it.Item().KeyCopy(nil)[len(prefix):])
			for partitionHashB64 := range evidata.InternalObjects {
				if pdata, err := readPartitionFileByHash(txn, partitionHashB64); err == nil {
					totalWork += int64(dbio.PartitionMemberCount(pdata))
				}
			}
		}
		return nil
	})
	if err != nil {
		return rep, err
	}

	bar := newEnrichmentProgressBar(totalWork, "enriching graphdb")
	defer func() {
		_ = bar.Finish()
		fmt.Fprintln(console.Err())
	}()
	for _, eid := range ids {
		evidata, err := dbio.GetEvidenceFile(eid, service.db)
		if err != nil {
			return rep, err
		}
		if err := service.enrichEvidence(ctx, eid, evidata, &rep, func() { _ = bar.Add(1) }); err != nil {
			return rep, err
		}
	}
	return rep, rep.err()
}

// EnrichEvidenceIndex upserts the hierarchy of one completed evidence (raw
// ID eid) into graphdb: the evidence, its partitions and their indexed files.
// An ingest runs it once the evidence is committed.
func (service *Service) EnrichEvidenceIndex(ctx context.Context, eid []byte) (Report, error) {
	var rep Report
	if service == nil || service.db == nil || service.repository == nil {
		return rep, nil
	}
	evidata, err := dbio.GetEvidenceFile(eid, service.db)
	if err != nil {
		return rep, err
	}
	if !evidata.Completed {
		return rep, cnst.ErrIncompleteFile
	}
	if err := service.enrichEvidence(ctx, eid, evidata, &rep, nil); err != nil {
		return rep, err
	}
	return rep, rep.err()
}

// partitionEntry is one partition of an evidence, as its record says.
type partitionEntry struct {
	idB64 string
	rec   structs.PartitionFile
	ok    bool
}

func (service *Service) enrichEvidence(ctx context.Context, eid []byte, evidata structs.EvidenceFile, rep *Report, onProcessed func()) error {
	evidence := evidenceContext{
		evidenceFileID:   base64.StdEncoding.EncodeToString(eid),
		evidenceFileName: evidata.Name,
	}
	// An index known to be wrong is hidden, as list hides it: only the
	// evidence itself is written.
	quarantined := evidata.Index != nil && evidata.Index.State == structs.IndexInvalid

	var parts []partitionEntry
	if !quarantined {
		err := service.db.View(func(txn *badger.Txn) error {
			for idB64 := range evidata.InternalObjects {
				pdata, err := readPartitionFileByHash(txn, idB64)
				parts = append(parts, partitionEntry{idB64: idB64, rec: pdata, ok: err == nil})
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	evEntropy, partEntropy, err := service.evidenceEntropy(ctx, eid, evidata, parts, rep)
	if err != nil {
		return err
	}

	// Acquisition format and ingest state are what tell an examiner whether
	// a thin subtree means "nothing was there" or "we never finished reading
	// it".
	if err := service.repository.UpsertEvidence(EvidenceRecord{
		HashBase64:     evidence.evidenceFileID,
		Name:           evidata.Name,
		Path:           evidencePath(evidata),
		Size:           evidata.Size,
		EvidenceType:   evidata.EvidenceType,
		IngestState:    string(evidata.IngestState),
		Completed:      evidata.Completed,
		Failed:         evidata.Failed,
		PartitionCount: len(evidata.InternalObjects),
	}); err != nil {
		return err
	}
	rep.Evidence++

	evidenceRecord := FileRecord{
		Level:            "evidence_file",
		Size:             evidata.Size,
		EvidenceFileID:   evidence.evidenceFileID,
		EvidenceFileName: evidence.evidenceFileName,
	}
	if evEntropy != nil {
		evidenceRecord.Entropy, evidenceRecord.HasEntropy = *evEntropy, true
	}
	if err := service.repository.UpsertFile(nameLevelRecord(evidenceRecord, evidata.Name, evidata.Name, false)); err != nil {
		return err
	}
	if quarantined {
		rep.Quarantined++
		return nil
	}

	cache := map[string]float64{}
	for i, p := range parts {
		if !p.ok {
			if err := service.upsertUnreadablePartition(evidence, p.idB64); err != nil {
				return err
			}
			continue
		}
		if err := service.enrichPartition(ctx, evidence, p, partEntropy[i], cache, rep, onProcessed); err != nil {
			return err
		}
	}
	return nil
}

// evidencePath is the path the evidence was stored from.
func evidencePath(e structs.EvidenceFile) string {
	if e.Source != nil && len(e.Source.Members) > 0 {
		return e.Source.Members[0].Path
	}
	return ""
}

// evidenceEntropy computes the entropy of an evidence and of each of its
// partitions in one pass over the evidence's stored chunks: partitions are
// ranges of it. Each partition used to be read again, after the evidence.
// Content that cannot be read is counted in rep; entries for it are nil.
func (service *Service) evidenceEntropy(ctx context.Context, eid []byte, e structs.EvidenceFile, parts []partitionEntry, rep *Report) (*float64, []*float64, error) {
	partValues := make([]*float64, len(parts))
	if e.Size == 0 {
		zero := 0.0
		for i := range partValues {
			partValues[i] = &zero
		}
		return &zero, partValues, nil
	}
	lg, err := dbio.OpenLogical(service.db, util.AppendToBytesSlice(cnst.EviFileNamespace, eid), dbio.ReadOptions{})
	if err == nil {
		var whole entropy.Counter
		counts := make([]entropy.Counter, len(parts))
		err = lg.Spans(ctx, dbio.SpanBatch(lg.ChunkSize()), func(spans []dbio.ChunkSpan) error {
			for _, s := range spans {
				data, err := lg.ReadSpan(s)
				if err != nil {
					return err
				}
				whole.Add(data)
				lo, hi := s.Offset, s.Offset+int64(len(data))
				for i, p := range parts {
					if !p.ok {
						continue
					}
					plo, phi := max(lo, p.rec.Start), min(hi, p.rec.Start+p.rec.Size)
					if plo < phi {
						counts[i].Add(data[plo-lo : phi-lo])
					}
				}
			}
			return nil
		})
		if err == nil {
			v := whole.Value()
			for i, p := range parts {
				if p.ok {
					pv := counts[i].Value()
					partValues[i] = &pv
				}
			}
			return &v, partValues, nil
		}
	}
	if ctx.Err() != nil {
		return nil, nil, context.Cause(ctx)
	}
	n := 1
	for _, p := range parts {
		if p.ok {
			n++
		}
	}
	rep.unreadable(n, "evidence "+e.Name, err)
	return nil, partValues, nil
}

func (service *Service) enrichPartition(ctx context.Context, evidence evidenceContext, p partitionEntry, partEntropy *float64, cache map[string]float64, rep *Report, onProcessed func()) error {
	pdata := p.rec
	name := dbio.RecordPath(pdata.IndexedFile)
	pc := partitionContext{
		evidenceContext: evidence,
		partitionID:     p.idB64,
		partitionName:   pdata.Name,
	}
	// Enrich the structural partition node before its files. Its offset, size,
	// filesystem type and hash are all parsed at ingest.
	if err := service.repository.UpsertPartition(PartitionRecord{
		EvidenceFileID:   evidence.evidenceFileID,
		EvidenceFileName: evidence.evidenceFileName,
		PartitionID:      p.idB64,
		PartitionName:    pdata.Name,
		Offset:           pdata.Start,
		Size:             pdata.Size,
		FilesystemType:   pdata.IndexedType,
		Hash:             pdata.FileHash,
		IndexedFileCount: dbio.PartitionMemberCount(pdata),
	}); err != nil {
		return err
	}
	rep.Partitions++

	partitionRecord := FileRecord{
		Level:            "partition",
		Size:             pdata.Size,
		FileType:         pdata.IndexedType,
		EvidenceFileID:   evidence.evidenceFileID,
		EvidenceFileName: evidence.evidenceFileName,
		PartitionID:      p.idB64,
		PartitionName:    pc.partitionName,
	}
	if partEntropy != nil {
		partitionRecord.Entropy, partitionRecord.HasEntropy = *partEntropy, true
	}
	// The partition record leads the batch so its node exists before the
	// indexed files that hang off it are written in the same commit.
	pending := make([]FileRecord, 0, fileBatchSize)
	pending = append(pending, nameLevelRecord(partitionRecord, name, name, false))

	pid, err := base64.StdEncoding.DecodeString(p.idB64)
	if err != nil {
		return err
	}
	var members []string
	err = service.db.View(func(txn *badger.Txn) error {
		return dbio.ForEachPartitionMemberTxn(txn, pid, pdata, func(indexedHashB64 string, _ structs.InternalOffset) error {
			members = append(members, indexedHashB64)
			return nil
		})
	})
	if err != nil {
		return err
	}
	for _, indexedHashB64 := range members {
		record, err := service.buildIndexedFileRecord(ctx, pc, indexedHashB64, cache, rep)
		if err != nil {
			return err
		}
		pending = append(pending, record)
		rep.Files++
		if pending, err = service.flushIfFull(pending); err != nil {
			return err
		}
		if onProcessed != nil {
			onProcessed()
		}
	}
	return service.upsertFiles(pending)
}

func newEnrichmentProgressBar(total int64, description string) *progressbar.ProgressBar {
	if total <= 0 {
		total = 1
	}
	return progressbar.NewOptions64(
		total,
		progressbar.OptionSetDescription(description),
		progressbar.OptionSetWriter(console.Progress()),
		progressbar.OptionSetTheme(cnst.CommonProgressBarTheme),
	)
}

// fileBatchSize bounds how many records are held before a commit.
//
// A partition can hold hundreds of thousands of files, so accumulating a whole
// one before writing would trade an fsync problem for a memory problem. This is
// large enough that the per-commit cost disappears and small enough that the
// working set stays trivial.
const fileBatchSize = 2000

// fileSimhash reads the file-level similarity fingerprint ingest already
// computed and cached.
//
// It has existed under the F||| namespace since the near-duplicate work, but
// only lib/near ever read it, so similarity was invisible to the graph. A miss
// is not an error: the fingerprint is written asynchronously during ingest, so
// a file enriched very soon after storage may legitimately not have one yet.
func (service *Service) fileSimhash(rawHash []byte) (uint64, bool) {
	if service == nil || service.db == nil || len(rawHash) == 0 {
		return 0, false
	}
	value, err := dbio.GetFileSimhash(rawHash, service.db)
	if err != nil || value == 0 {
		return 0, false
	}
	return value, true
}

// upsertFiles writes records in one commit where the backend supports it.
func (service *Service) upsertFiles(records []FileRecord) error {
	if len(records) == 0 {
		return nil
	}
	if batcher, ok := service.repository.(FileBatchRepository); ok {
		return batcher.UpsertFiles(records)
	}
	for _, record := range records {
		if err := service.repository.UpsertFile(record); err != nil {
			return err
		}
	}
	return nil
}

// flushIfFull commits the pending records once they reach the batch size,
// returning the slice to keep accumulating into.
func (service *Service) flushIfFull(pending []FileRecord) ([]FileRecord, error) {
	if len(pending) < fileBatchSize {
		return pending, nil
	}
	if err := service.upsertFiles(pending); err != nil {
		return nil, err
	}
	return pending[:0], nil
}

func (service *Service) buildIndexedFileRecord(ctx context.Context, pc partitionContext, indexedHashB64 string, cache map[string]float64, rep *Report) (FileRecord, error) {
	rawIndexedHash, indexedHashCanonical := parseHashValue(indexedHashB64)
	indexedID := util.AppendToBytesSlice(cnst.IdxFileNamespace, rawIndexedHash)
	indexedFile, err := dbio.GetIndexedFile(indexedID, service.db)
	if err != nil {
		return FileRecord{}, fmt.Errorf("get indexed file: %w", err)
	}
	filePath := dbio.RecordPath(indexedFile)
	record := FileRecord{
		Level:            "indexed_file",
		Size:             indexedFile.Size,
		FileType:         indexedFile.IndexedType,
		EvidenceFileID:   pc.evidenceFileID,
		EvidenceFileName: pc.evidenceFileName,
		PartitionID:      pc.partitionID,
		PartitionName:    pc.partitionName,
		IndexedFileID:    indexedHashCanonical,
		IndexedFileHash:  indexedFile.FileHash,
		Times:            indexedFile.Times,
		Start:            indexedFile.Start,
		IsFragmented:     indexedFile.IsFragmented,
	}
	record.Entropy, record.HasEntropy, err = service.fileEntropy(ctx, indexedID, indexedFile, filePath, cache, rep)
	if err != nil {
		return FileRecord{}, err
	}
	record.Simhash, record.HasSimhash = service.fileSimhash(rawIndexedHash)

	return nameLevelRecord(record, path.Base(filePath), filePath, indexedFile.IsDeleted), nil
}

// fileEntropy is the entropy the index pass recorded, or, for a record
// indexed without it, the entropy of the content read now. Content that
// cannot be read is counted in rep; a file with no content to read (a
// symlink, or content the index could only describe) has none.
func (service *Service) fileEntropy(ctx context.Context, key []byte, rec structs.IndexedFile, what string, cache map[string]float64, rep *Report) (float64, bool, error) {
	if rec.Entropy != nil {
		return *rec.Entropy, true, nil
	}
	if rec.Content != nil && rec.Content.Kind == structs.ContentUnavailable {
		return 0, false, nil
	}
	if v, ok := cache[rec.FileHash]; ok && rec.FileHash != "" {
		return v, true, nil
	}
	var counts entropy.Counter
	lg, err := dbio.OpenLogical(service.db, key, dbio.ReadOptions{})
	if err == nil {
		err = lg.Stream(ctx, func(p []byte) error {
			counts.Add(p)
			return nil
		})
	}
	switch {
	case err == nil:
		v := counts.Value()
		if rec.FileHash != "" {
			cache[rec.FileHash] = v
		}
		return v, true, nil
	case errors.Is(err, dbio.ErrContentUnavailable):
		return 0, false, nil
	case ctx.Err() != nil:
		return 0, false, context.Cause(ctx)
	default:
		rep.unreadable(1, what, err)
		return 0, false, nil
	}
}

func parseHashValue(value string) ([]byte, string) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err == nil {
		return raw, value
	}

	raw = []byte(value)
	return raw, base64.StdEncoding.EncodeToString(raw)
}

func decodeStoredValue(value []byte) []byte {
	decoded, err := cnst.DECODER.DecodeAll(value, nil)
	if err == nil {
		return decoded
	}
	return value
}

func readPartitionFileByHash(txn *badger.Txn, partitionHashB64 string) (structs.PartitionFile, error) {
	var pdata structs.PartitionFile

	rawPartitionHash, err := base64.StdEncoding.DecodeString(partitionHashB64)
	if err != nil {
		return pdata, fmt.Errorf("partition hash decode: %w", err)
	}
	partitionID := util.AppendToBytesSlice(cnst.PartiFileNamespace, rawPartitionHash)

	item, err := txn.Get(partitionID)
	if err != nil {
		return pdata, fmt.Errorf("get partition %s: %w", partitionHashB64, err)
	}
	value, err := item.ValueCopy(nil)
	if err != nil {
		return pdata, err
	}
	value = decodeStoredValue(value)

	if err := msgpack.Unmarshal(value, &pdata); err != nil {
		return pdata, err
	}

	return pdata, nil
}

// nameLevelRecord names a file record. Records are named from their own
// fields through the record helpers, not by splitting legacy names on the
// separator, which broke names holding it (SRC-8).
func nameLevelRecord(shared FileRecord, name, filePath string, deleted bool) FileRecord {
	record := shared
	record.FileName = strings.TrimSpace(name)
	if record.FileName == "" || record.FileName == "." || record.FileName == "/" {
		record.FileName = filePath
	}
	record.Path = filePath
	record.IsDeleted = deleted
	record.MimeType, record.Tags = classifyFile(record.Path, record.FileType)
	return record
}

func (service *Service) upsertUnreadablePartition(evidence evidenceContext, partitionHashB64 string) error {
	if service == nil || service.repository == nil {
		return nil
	}

	return service.repository.UpsertPartition(PartitionRecord{
		EvidenceFileID:   evidence.evidenceFileID,
		EvidenceFileName: evidence.evidenceFileName,
		PartitionID:      partitionHashB64,
		PartitionName:    partitionHashB64,
	})
}
