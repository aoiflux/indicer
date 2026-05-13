package enrichment

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"math"
	"os"
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
		fmt.Fprintln(os.Stderr)
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

			decoded, err := cnst.DECODER.DecodeAll(value, nil)
			if err == nil {
				value = decoded
			}

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
				evidenceFileName: util.GetArbitratyMapKey(evidata.Names),
			}

			// Create evidence-file-level file records for each file at evidence level
			ensureIndexedNameMeta(&evidata.IndexedFile)
			evidenceEntropy, hasEvidenceEntropy := service.computeEntropyForLogicalFile(cnst.EviFileNamespace, context.evidenceFileID, entropyCache)
			for evidenceFileName := range evidata.Names {
				fileName := evidenceFileName
				if strings.Contains(evidenceFileName, cnst.DataSeperator) {
					parts := strings.SplitN(evidenceFileName, cnst.DataSeperator, 3)
					if len(parts) >= 3 {
						fileName = parts[2]
					}
				}
				meta := evidata.NameMeta[evidenceFileName]

				evidenceFileRecord := FileRecord{
					Level:            "evidence_file",
					FileName:         fileName,
					Path:             evidenceFileName,
					Size:             evidata.Size,
					IsDeleted:        meta.IsDeleted,
					IsFragmented:     meta.IsFragmented,
					EvidenceFileID:   context.evidenceFileID,
					EvidenceFileName: context.evidenceFileName,
					Entropy:          evidenceEntropy,
					HasEntropy:       hasEvidenceEntropy,
				}
				evidenceFileRecord.MimeType, evidenceFileRecord.Tags = classifyFile(evidenceFileRecord.Path, evidenceFileRecord.FileType)
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
func (service *Service) EnrichPartition(pfile structs.InputFile, indexedHashes []string) error {
	if service == nil || service.db == nil || service.repository == nil || len(indexedHashes) == 0 {
		return nil
	}

	bar := newEnrichmentProgressBar(int64(len(indexedHashes)), "Enriching graphdb")
	defer func() {
		bar.Finish()
		fmt.Fprintln(os.Stderr)
	}()

	partitionIDBytes, err := pfile.GetEncodedHash()
	if err != nil {
		return err
	}
	partitionID := string(partitionIDBytes)
	partitionName := pfile.GetName()

	evidenceID := string(pfile.GetEviFileHash())
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

	for _, indexedHashB64 := range indexedHashes {
		if err := service.enrichIndexedHash(context, indexedHashB64, entropyCache); err != nil {
			return err
		}
		_ = bar.Add(1)
	}
	return nil
}

func (service *Service) enrichPartitionByHash(txn *badger.Txn, evidence evidenceContext, partitionHashB64 string, entropyCache map[string]float64, onProcessed func()) error {
	rawPartitionHash, err := base64.StdEncoding.DecodeString(partitionHashB64)
	if err != nil {
		return fmt.Errorf("partition hash decode: %w", err)
	}
	partitionID := util.AppendToBytesSlice(cnst.PartiFileNamespace, rawPartitionHash)

	item, err := txn.Get(partitionID)
	if err != nil {
		return fmt.Errorf("get partition %s: %w", partitionHashB64, err)
	}
	value, err := item.ValueCopy(nil)
	if err != nil {
		return err
	}
	decoded, err := cnst.DECODER.DecodeAll(value, nil)
	if err == nil {
		value = decoded
	}

	var pdata structs.PartitionFile
	if err := msgpack.Unmarshal(value, &pdata); err != nil {
		return err
	}

	context := partitionContext{
		evidenceContext: evidence,
		partitionID:     partitionHashB64,
		partitionName:   util.GetArbitratyMapKey(pdata.Names),
	}
	partitionEntropy, hasPartitionEntropy := service.computeEntropyForLogicalFile(cnst.PartiFileNamespace, partitionHashB64, entropyCache)

	// Create partition-level file records for each file in this partition
	ensureIndexedNameMeta(&pdata.IndexedFile)
	for partitionFileName := range pdata.Names {
		fileName := partitionFileName
		if strings.Contains(partitionFileName, cnst.DataSeperator) {
			parts := strings.SplitN(partitionFileName, cnst.DataSeperator, 3)
			if len(parts) >= 3 {
				fileName = parts[2]
			}
		}
		meta := pdata.NameMeta[partitionFileName]

		partitionRecord := FileRecord{
			Level:            "partition",
			FileName:         fileName,
			Path:             partitionFileName,
			Size:             pdata.Size,
			IsDeleted:        meta.IsDeleted,
			IsFragmented:     meta.IsFragmented,
			FileType:         pdata.IndexedType,
			EvidenceFileID:   evidence.evidenceFileID,
			EvidenceFileName: evidence.evidenceFileName,
			PartitionID:      partitionHashB64,
			PartitionName:    context.partitionName,
			Entropy:          partitionEntropy,
			HasEntropy:       hasPartitionEntropy,
		}
		partitionRecord.MimeType, partitionRecord.Tags = classifyFile(partitionRecord.Path, partitionRecord.FileType)
		if err := service.repository.UpsertFile(partitionRecord); err != nil {
			return err
		}
	}

	// Process indexed files within this partition
	for indexedHashB64 := range pdata.InternalObjects {
		if err := service.enrichIndexedHash(context, indexedHashB64, entropyCache); err != nil {
			return err
		}
		if onProcessed != nil {
			onProcessed()
		}
	}
	return nil
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

			decoded, err := cnst.DECODER.DecodeAll(value, nil)
			if err == nil {
				value = decoded
			}

			var evidata structs.EvidenceFile
			if err := msgpack.Unmarshal(value, &evidata); err != nil {
				return err
			}
			if !evidata.Completed {
				continue
			}

			for partitionHashB64 := range evidata.InternalObjects {
				rawPartitionHash, err := base64.StdEncoding.DecodeString(partitionHashB64)
				if err != nil {
					return fmt.Errorf("partition hash decode: %w", err)
				}
				partitionID := util.AppendToBytesSlice(cnst.PartiFileNamespace, rawPartitionHash)

				partitionItem, err := txn.Get(partitionID)
				if err != nil {
					return fmt.Errorf("get partition %s: %w", partitionHashB64, err)
				}
				partitionValue, err := partitionItem.ValueCopy(nil)
				if err != nil {
					return err
				}
				partitionDecoded, err := cnst.DECODER.DecodeAll(partitionValue, nil)
				if err == nil {
					partitionValue = partitionDecoded
				}

				var pdata structs.PartitionFile
				if err := msgpack.Unmarshal(partitionValue, &pdata); err != nil {
					return err
				}
				total += int64(len(pdata.InternalObjects))
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
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionSetTheme(cnst.CommonProgressBarTheme),
	)
}

func (service *Service) enrichIndexedHash(context partitionContext, indexedHashB64 string, entropyCache map[string]float64) error {
	rawIndexedHash, indexedHashCanonical := parseHashValue(indexedHashB64)
	indexedID := util.AppendToBytesSlice(cnst.IdxFileNamespace, rawIndexedHash)
	indexedFile, err := dbio.GetIndexedFile(indexedID, service.db)
	if err != nil {
		return fmt.Errorf("get indexed file: %w", err)
	}
	ensureIndexedNameMeta(&indexedFile)
	indexedEntropy, hasIndexedEntropy := service.computeEntropyForLogicalFile(cnst.IdxFileNamespace, indexedHashCanonical, entropyCache)

	// Create one FileRecord per file NAME at the indexed file level
	for indexedName := range indexedFile.Names {
		fileName, filePath := splitIndexedName(indexedName)
		meta := indexedFile.NameMeta[indexedName]

		record := FileRecord{
			Level:            "indexed_file",
			FileName:         fileName,
			Path:             filePath,
			Size:             indexedFile.Size,
			IsDeleted:        indexedFile.IsDeleted || meta.IsDeleted,
			IsFragmented:     meta.IsFragmented,
			FileType:         indexedFile.IndexedType,
			EvidenceFileID:   context.evidenceFileID,
			EvidenceFileName: context.evidenceFileName,
			PartitionID:      context.partitionID,
			PartitionName:    context.partitionName,
			IndexedFileID:    indexedHashCanonical,
			IndexedFileHash:  indexedHashCanonical,
			Entropy:          indexedEntropy,
			HasEntropy:       hasIndexedEntropy,
		}
		record.MimeType, record.Tags = classifyFile(record.Path, record.FileType)
		if err := service.repository.UpsertFile(record); err != nil {
			return err
		}
	}

	return nil
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

func ensureIndexedNameMeta(indexedFile *structs.IndexedFile) {
	if indexedFile == nil {
		return
	}
	if indexedFile.NameMeta == nil {
		indexedFile.NameMeta = make(map[string]structs.IndexedNameMeta)
	}
	for name := range indexedFile.Names {
		if _, ok := indexedFile.NameMeta[name]; ok {
			continue
		}
		indexedFile.NameMeta[name] = structs.IndexedNameMeta{IsDeleted: indexedFile.IsDeleted}
	}
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
