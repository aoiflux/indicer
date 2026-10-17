package enrichment

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"indicer/internal/console"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"math"
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

type evidenceContext struct {
	evidenceFileID   string
	evidenceFileName string
}

type partitionContext struct {
	evidenceContext
	partitionID   string
	partitionName string
}

// EnrichAll scans all completed evidence and upserts file hierarchy metadata
// into graphdb: evidence file -> partition -> indexed file nodes.
func (service *Service) EnrichAll() error {
	if service == nil || service.db == nil || service.repository == nil {
		return nil
	}

	totalWork, err := service.countIndexedObjectsForEnrichment()
	if err != nil {
		return err
	}
	bar := newEnrichmentProgressBar(totalWork, "enriching graphdb")
	defer func() {
		bar.Finish()
		fmt.Fprintln(console.Err())
	}()
	entropyCache := make(map[string]float64)

	return service.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 100
		it := txn.NewIterator(opts)
		defer it.Close()

		evidencePrefix := []byte(cnst.EviFileNamespace)
		for it.Seek(evidencePrefix); it.ValidForPrefix(evidencePrefix); it.Next() {
			item := it.Item()
			key := item.KeyCopy(nil)
			value, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}
			value = decodeStoredValue(value)

			var evidata structs.EvidenceFile
			if err := msgpack.Unmarshal(value, &evidata); err != nil {
				return err
			}
			if !evidata.Completed {
				continue
			}

			evidenceHash := bytes.TrimPrefix(key, evidencePrefix)
			context := evidenceContext{
				evidenceFileID:   base64.StdEncoding.EncodeToString(evidenceHash),
				evidenceFileName: evidata.Name,
			}

			// The structural evidence node used to be created as a stub
			// carrying only an id and a name, because nothing called
			// UpsertEvidence on this path. Acquisition format and ingest state
			// are what tell an examiner whether a thin subtree means "nothing
			// was there" or "we never finished reading it".
			if err := service.repository.UpsertEvidence(EvidenceRecord{
				HashBase64:     context.evidenceFileID,
				Name:           evidata.Name,
				Size:           evidata.Size,
				EvidenceType:   evidata.EvidenceType,
				IngestState:    string(evidata.IngestState),
				Completed:      evidata.Completed,
				Failed:         evidata.Failed,
				PartitionCount: len(evidata.InternalObjects),
			}); err != nil {
				return err
			}

			evidenceEntropy, hasEvidenceEntropy := service.computeEntropyForLogicalFile(cnst.EviFileNamespace, context.evidenceFileID, entropyCache)
			sharedEvidenceRecord := FileRecord{
				Level:            "evidence_file",
				Size:             evidata.Size,
				EvidenceFileID:   context.evidenceFileID,
				EvidenceFileName: context.evidenceFileName,
				Entropy:          evidenceEntropy,
				HasEntropy:       hasEvidenceEntropy,
			}
			if evidenceFileRecord := buildNameLevelRecord(sharedEvidenceRecord, evidata.Name, false); true {
				if err := service.repository.UpsertFile(evidenceFileRecord); err != nil {
					return err
				}
			}

			for partitionHashB64 := range evidata.InternalObjects {
				if err := service.enrichPartitionByHash(txn, context, partitionHashB64, entropyCache, func() {
					_ = bar.Add(1)
				}); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// EnrichPartition enriches graphdb metadata for one partition worth of indexed
// hashes during store-time indexing.
func (service *Service) EnrichPartition(pfile structs.InputFile, eviID []byte, indexedHashes []string) error {
	if service == nil || service.db == nil || service.repository == nil || len(indexedHashes) == 0 {
		return nil
	}

	bar := newEnrichmentProgressBar(int64(len(indexedHashes)), "Enriching graphdb")
	defer func() {
		bar.Finish()
		fmt.Fprintln(console.Err())
	}()

	partitionID := base64.StdEncoding.EncodeToString(pfile.GetID())
	partitionName := pfile.GetName()

	evidenceID := string(eviID)
	evidenceName := partitionName
	parts := strings.SplitN(partitionName, cnst.DataSeperator, 3)
	if len(parts) >= 2 && parts[1] != "" {
		evidenceName = parts[1]
	}

	context := partitionContext{
		evidenceContext: evidenceContext{evidenceFileID: evidenceID, evidenceFileName: evidenceName},
		partitionID:     partitionID,
		partitionName:   partitionName,
	}
	entropyCache := make(map[string]float64)

	pending := make([]FileRecord, 0, fileBatchSize)
	for _, indexedHashB64 := range indexedHashes {
		record, err := service.buildIndexedFileRecord(context, indexedHashB64, entropyCache)
		if err != nil {
			return err
		}
		pending = append(pending, record)
		if pending, err = service.flushIfFull(pending); err != nil {
			return err
		}
		_ = bar.Add(1)
	}
	return service.upsertFiles(pending)
}

func (service *Service) enrichPartitionByHash(txn *badger.Txn, evidence evidenceContext, partitionHashB64 string, entropyCache map[string]float64, onProcessed func()) error {
	pdata, err := readPartitionFileByHash(txn, partitionHashB64)
	if err != nil {
		return service.upsertUnreadablePartition(evidence, partitionHashB64)
	}

	context := partitionContext{
		evidenceContext: evidence,
		partitionID:     partitionHashB64,
		partitionName:   pdata.Name,
	}
	// Enrich the structural partition node before its files. Its offset, size,
	// filesystem type and hash are all parsed at ingest and were being dropped,
	// which left partition nodes carrying nothing but an id and a name.
	if err := service.repository.UpsertPartition(PartitionRecord{
		EvidenceFileID:   evidence.evidenceFileID,
		EvidenceFileName: evidence.evidenceFileName,
		PartitionID:      partitionHashB64,
		PartitionName:    pdata.Name,
		Offset:           pdata.Start,
		Size:             pdata.Size,
		FilesystemType:   pdata.IndexedType,
		Hash:             pdata.FileHash,
		IndexedFileCount: dbio.PartitionMemberCount(pdata),
	}); err != nil {
		return err
	}

	partitionEntropy, hasPartitionEntropy := service.computeEntropyForLogicalFile(cnst.PartiFileNamespace, partitionHashB64, entropyCache)

	sharedPartitionRecord := FileRecord{
		Level:            "partition",
		Size:             pdata.Size,
		FileType:         pdata.IndexedType,
		EvidenceFileID:   evidence.evidenceFileID,
		EvidenceFileName: evidence.evidenceFileName,
		PartitionID:      partitionHashB64,
		PartitionName:    context.partitionName,
		Entropy:          partitionEntropy,
		HasEntropy:       hasPartitionEntropy,
	}
	// The partition record leads the batch so its node exists before the
	// indexed files that hang off it are written in the same commit.
	pending := make([]FileRecord, 0, fileBatchSize)
	pending = append(pending, buildNameLevelRecord(sharedPartitionRecord, pdata.Name, false))

	pid, err := base64.StdEncoding.DecodeString(partitionHashB64)
	if err != nil {
		return err
	}
	err = dbio.ForEachPartitionMemberTxn(txn, pid, pdata, func(indexedHashB64 string, _ structs.InternalOffset) error {
		record, err := service.buildIndexedFileRecord(context, indexedHashB64, entropyCache)
		if err != nil {
			return err
		}
		pending = append(pending, record)
		if pending, err = service.flushIfFull(pending); err != nil {
			return err
		}
		if onProcessed != nil {
			onProcessed()
		}
		return nil
	})
	if err != nil {
		return err
	}
	return service.upsertFiles(pending)
}

func (service *Service) countIndexedObjectsForEnrichment() (int64, error) {
	var total int64
	err := service.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 100
		it := txn.NewIterator(opts)
		defer it.Close()

		evidencePrefix := []byte(cnst.EviFileNamespace)
		for it.Seek(evidencePrefix); it.ValidForPrefix(evidencePrefix); it.Next() {
			item := it.Item()
			value, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}
			value = decodeStoredValue(value)

			var evidata structs.EvidenceFile
			if err := msgpack.Unmarshal(value, &evidata); err != nil {
				return err
			}
			if !evidata.Completed {
				continue
			}

			for partitionHashB64 := range evidata.InternalObjects {
				pdata, err := readPartitionFileByHash(txn, partitionHashB64)
				if err != nil {
					continue
				}
				total += int64(dbio.PartitionMemberCount(pdata))
			}
		}

		return nil
	})
	if err != nil {
		return 0, err
	}
	if total <= 0 {
		total = 1
	}
	return total, nil
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

func (service *Service) buildIndexedFileRecord(context partitionContext, indexedHashB64 string, entropyCache map[string]float64) (FileRecord, error) {
	rawIndexedHash, indexedHashCanonical := parseHashValue(indexedHashB64)
	indexedID := util.AppendToBytesSlice(cnst.IdxFileNamespace, rawIndexedHash)
	indexedFile, err := dbio.GetIndexedFile(indexedID, service.db)
	if err != nil {
		return FileRecord{}, fmt.Errorf("get indexed file: %w", err)
	}
	indexedEntropy, hasIndexedEntropy := service.computeEntropyForLogicalFile(cnst.IdxFileNamespace, indexedHashCanonical, entropyCache)
	sharedIndexedRecord := FileRecord{
		Level:            "indexed_file",
		Size:             indexedFile.Size,
		FileType:         indexedFile.IndexedType,
		EvidenceFileID:   context.evidenceFileID,
		EvidenceFileName: context.evidenceFileName,
		PartitionID:      context.partitionID,
		PartitionName:    context.partitionName,
		IndexedFileID:    indexedHashCanonical,
		IndexedFileHash:  indexedFile.FileHash,
		Entropy:          indexedEntropy,
		HasEntropy:       hasIndexedEntropy,
		Times:            indexedFile.Times,
		Start:            indexedFile.Start,
		IsFragmented:     indexedFile.IsFragmented,
	}
	sharedIndexedRecord.Simhash, sharedIndexedRecord.HasSimhash =
		service.fileSimhash(rawIndexedHash)

	return buildNameLevelRecord(sharedIndexedRecord, indexedFile.Name, indexedFile.IsDeleted), nil
}

func (service *Service) computeEntropyForLogicalFile(namespace, encodedHash string, cache map[string]float64) (float64, bool) {
	if service == nil || service.db == nil || encodedHash == "" {
		return 0, false
	}
	cacheKey := namespace + encodedHash
	if entropy, ok := cache[cacheKey]; ok {
		return entropy, true
	}
	rawHash, err := base64.StdEncoding.DecodeString(encodedHash)
	if err != nil {
		return 0, false
	}
	fid := util.AppendToBytesSlice(namespace, rawHash)
	entropy, err := computeLogicalFileEntropy(fid, service.db)
	if err != nil {
		return 0, false
	}
	cache[cacheKey] = entropy
	return entropy, true
}

func computeLogicalFileEntropy(fid []byte, db *badger.DB) (float64, error) {
	var counts [256]uint64
	var total uint64
	err := dbio.StreamLogicalFileBytes(fid, db, func(chunk []byte) error {
		for _, b := range chunk {
			counts[b]++
			total++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if total == 0 {
		return 0, nil
	}
	invTotal := 1.0 / float64(total)
	entropy := 0.0
	for _, count := range counts {
		if count == 0 {
			continue
		}
		p := float64(count) * invTotal
		entropy -= p * math.Log2(p)
	}
	return entropy, nil
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

func buildNameLevelRecord(shared FileRecord, rawName string, baseDeleted bool) FileRecord {
	name, path := splitIndexedName(rawName)
	record := shared
	record.FileName = name
	record.Path = path
	record.IsDeleted = baseDeleted
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

func splitIndexedName(indexedName string) (name string, path string) {
	if strings.Contains(indexedName, cnst.DataSeperator) {
		parts := strings.SplitN(indexedName, cnst.DataSeperator, 3)
		if len(parts) == 3 {
			return parts[2], indexedName
		}
	}
	return indexedName, indexedName
}
