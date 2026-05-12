package search

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/fts"
	"indicer/lib/near"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/schollz/progressbar/v3"
)

var searchNamespaces = []string{
	cnst.IdxFileNamespace,
	cnst.PartiFileNamespace,
	cnst.EviFileNamespace,
}

const defaultReportPath = "report.json"

type searchSettings struct {
	query      string
	queryBytes []byte
	queryLen   int
}

func Search(query string, db *badger.DB) error {
	return SearchWithContext(context.Background(), query, db)
}

func newSearchSettings(query string) searchSettings {
	return searchSettings{
		query:      query,
		queryBytes: bytes.ToLower([]byte(query)),
		queryLen:   len(query),
	}
}

func workerLimit() int {
	maxWorkers := cnst.GetMaxThreadCount()
	if maxWorkers <= 0 {
		return 1
	}
	return maxWorkers
}

func SearchSummaryWithContext(ctx context.Context, query string, db *badger.DB) (map[string]int64, int64, error) {
	if len(query) < 2 {
		return nil, 0, cnst.ErrSmallQuery
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	settings := newSearchSettings(query)
	idmap, err := executeSearchPipeline(ctx, settings, db, nil, nil)
	if err != nil {
		return nil, 0, err
	}

	results := idmap.GetData()
	return buildKeywordCountMap(results), countTotalOccurrences(results), nil
}

func SearchWithContext(ctx context.Context, query string, db *badger.DB) error {
	return SearchToPathWithContext(ctx, query, defaultReportPath, db)
}

// SearchWithContextFullTextFallback tries FTS-assisted search first and falls back
// to the scan-only path when the sidecar index is unavailable or not useful.
func SearchWithContextFullTextFallback(ctx context.Context, query string, db *badger.DB) error {
	return SearchToPathWithContextFullTextFallback(ctx, query, defaultReportPath, db)
}

func SearchToPathWithContextFullTextFallback(ctx context.Context, query, reportPath string, db *badger.DB) error {
	if reportPath == "" {
		reportPath = defaultReportPath
	}

	pq := parseQuery(query)
	if err := validateParsedQuery(pq); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	start := time.Now()
	totalFiles, err := countSearchFiles(ctx, db)
	if err != nil {
		return fmt.Errorf("countSearchFiles: %w", err)
	}

	ftScores, err := fts.SearchFileScores(ctx, db, query, 5000)
	if errors.Is(err, fts.ErrIndexNotReady) {
		if backfillErr := fts.BuildFromIndexedFiles(ctx, db); backfillErr == nil {
			ftScores, err = fts.SearchFileScores(ctx, db, query, 5000)
		}
	}
	if err != nil || len(ftScores) == 0 {
		return SearchToPathWithContext(ctx, query, reportPath, db)
	}

	candidates := fts.TopCandidateIDs(ftScores)
	totalWork := int64(len(candidates))*int64(len(pq.terms)) + 1
	if totalWork <= 0 {
		totalWork = 1
	}
	bar := progressbar.NewOptions64(
		totalWork,
		progressbar.OptionSetDescription("Searching...."),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "#",
			SaucerHead:    ">",
			SaucerPadding: "-",
			BarStart:      "[",
			BarEnd:        "]",
		}),
	)
	var progressMu sync.Mutex
	onProcessed := func() {
		progressMu.Lock()
		defer progressMu.Unlock()
		_ = bar.Add(1)
	}

	hitMaps, err := runMultiTermSearch(ctx, pq, db, onProcessed, candidates)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	hasHits := false
	for _, hm := range hitMaps {
		if len(hm) > 0 {
			hasHits = true
			break
		}
	}
	if !hasHits {
		return SearchToPathWithContext(ctx, query, reportPath, db)
	}

	docSizes, err := fetchDocSizesForHits(ctx, hitMaps, db)
	if err != nil {
		return err
	}

	ranked := rankBM25(hitMaps, docSizes, totalFiles, pq.op)
	ranked = applyFTSBoost(ranked, ftScores)

	err = searchReport(reportPath, query, ranked, db)
	if err != nil {
		return err
	}
	onProcessed()

	bar.Finish()
	fmt.Fprintln(os.Stderr)
	fmt.Println("Done....", time.Since(start))
	return bar.Close()
}

func SearchToPathWithContext(ctx context.Context, query, reportPath string, db *badger.DB) error {
	if reportPath == "" {
		reportPath = defaultReportPath
	}

	pq := parseQuery(query)
	if err := validateParsedQuery(pq); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	start := time.Now()
	totalFiles, err := countSearchFiles(ctx, db)
	if err != nil {
		return fmt.Errorf("countSearchFiles: %w", err)
	}

	totalWork := totalFiles*int64(len(pq.terms)) + 1
	if totalWork <= 0 {
		totalWork = 1
	}
	bar := progressbar.NewOptions64(
		totalWork,
		progressbar.OptionSetDescription("Searching...."),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "#",
			SaucerHead:    ">",
			SaucerPadding: "-",
			BarStart:      "[",
			BarEnd:        "]",
		}),
	)
	var progressMu sync.Mutex
	onProcessed := func() {
		progressMu.Lock()
		defer progressMu.Unlock()
		_ = bar.Add(1)
	}

	hitMaps, err := runMultiTermSearch(ctx, pq, db, onProcessed, nil)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	docSizes, err := fetchDocSizesForHits(ctx, hitMaps, db)
	if err != nil {
		return err
	}

	ranked := rankBM25(hitMaps, docSizes, totalFiles, pq.op)

	err = searchReport(reportPath, query, ranked, db)
	if err != nil {
		return err
	}
	onProcessed()

	bar.Finish()
	fmt.Fprintln(os.Stderr)
	fmt.Println("Done....", time.Since(start))
	return bar.Close()
}

func validateParsedQuery(pq parsedQuery) error {
	if len(pq.terms) == 0 {
		return cnst.ErrSmallQuery
	}
	for _, t := range pq.terms {
		if len(t) < 2 {
			return cnst.ErrSmallQuery
		}
	}
	return nil
}

func runMultiTermSearch(ctx context.Context, pq parsedQuery, db *badger.DB, onProcessed func(), candidates map[string]struct{}) ([]map[string]int, error) {
	hitMaps := make([]map[string]int, 0, len(pq.terms))
	for _, term := range pq.terms {
		settings := newSearchSettings(term)
		idmap, err := executeSearchPipeline(ctx, settings, db, onProcessed, candidates)
		if err != nil {
			return nil, err
		}
		hitMaps = append(hitMaps, idmap.GetData())
	}
	return hitMaps, nil
}

func fetchDocSizesForHits(ctx context.Context, hitMaps []map[string]int, db *badger.DB) (map[string]int64, error) {
	seen := make(map[string]struct{})
	for _, hits := range hitMaps {
		for id := range hits {
			seen[id] = struct{}{}
		}
	}
	sizes := make(map[string]int64, len(seen))
	for id := range seen {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		meta, err := store.GetFileMeta([]byte(id), db)
		if err != nil {
			sizes[id] = 0
			continue
		}
		sizes[id] = meta.Size
	}
	return sizes, nil
}

func applyFTSBoost(ranked []RankedResult, ftScores map[string]float64) []RankedResult {
	if len(ranked) == 0 || len(ftScores) == 0 {
		return ranked
	}
	for index := range ranked {
		if score, ok := ftScores[ranked[index].FileID]; ok {
			ranked[index].Score += 0.05 * score
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		if ranked[i].TF != ranked[j].TF {
			return ranked[i].TF > ranked[j].TF
		}
		return ranked[i].FileID < ranked[j].FileID
	})
	return ranked
}

func executeSearchPipeline(ctx context.Context, settings searchSettings, db *badger.DB, onProcessed func(), candidates map[string]struct{}) (*structs.SearchIDMap, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmap := structs.NewSeenChonkMap()
	idmap := structs.NewSearchIDMap()

	for _, namespace := range searchNamespaces {
		err := searchFiles(ctx, cancel, settings, namespace, cmap, idmap, db, onProcessed, candidates)
		if err != nil {
			return nil, err
		}
	}

	return idmap, nil
}

func countSearchFiles(ctx context.Context, db *badger.DB) (int64, error) {
	var total int64
	for _, namespace := range searchNamespaces {
		count, err := countNamespaceFiles(ctx, namespace, db)
		if err != nil {
			return 0, err
		}
		total += count
	}
	return total, nil
}

func countNamespaceFiles(ctx context.Context, namespace string, db *badger.DB) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	var count int64
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.PrefetchSize = 1000
		it := txn.NewIterator(opts)
		defer it.Close()

		prefix := []byte(namespace)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

func searchFiles(ctx context.Context, cancel context.CancelFunc, settings searchSettings, namespace string, cmap *structs.SeenChonkMap, idmap *structs.SearchIDMap, db *badger.DB, onProcessed func(), candidates map[string]struct{}) error {
	sem := make(chan struct{}, workerLimit())
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.PrefetchSize = 1000
		it := txn.NewIterator(opts)
		defer it.Close()

		prefix := []byte(namespace)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}

			fid := it.Item().KeyCopy(nil)
			if candidates != nil {
				if _, ok := candidates[string(fid)]; !ok {
					continue
				}
			}
			sem <- struct{}{}
			wg.Add(1)
			go func(fid []byte) {
				defer wg.Done()
				defer func() { <-sem }()

				workerErr := ctx.Err()
				if workerErr == nil {
					workerErr = searchAllFiles(ctx, cancel, settings, fid, cmap, idmap, db)
				}

				if onProcessed != nil {
					onProcessed()
				}
				if workerErr != nil {
					once.Do(func() {
						firstErr = workerErr
						cancel()
					})
				}
			}(fid)
		}

		return nil
	})

	wg.Wait()
	if err != nil {
		if firstErr != nil {
			return firstErr
		}
		return err
	}
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	return nil
}

func searchAllFiles(ctx context.Context, cancel context.CancelFunc, settings searchSettings, fid []byte, cmap *structs.SeenChonkMap, idmap *structs.SearchIDMap, db *badger.DB) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	meta, err := store.GetFileMeta(fid, db)
	if err != nil {
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		return fmt.Errorf("GetFileMeta for key %q: %w", string(fid), err)
	}
	return searchChonks(ctx, settings, string(fid), cmap, idmap, meta, db, cancel)
}

func searchChonks(ctx context.Context, settings searchSettings, fidStr string, cmap *structs.SeenChonkMap, idmap *structs.SearchIDMap, meta structs.FileMeta, db *badger.DB, cancel context.CancelFunc) error {
	var dbstart int64
	if meta.Start > 0 {
		dbstart = util.GetDBStartOffset(meta.Start)
	}
	end := meta.Start + meta.Size

	sem := make(chan struct{}, workerLimit())
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	for sindex := dbstart; sindex < end; sindex += cnst.ChonkSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(sindex int64) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := ctx.Err(); err != nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
				return
			}

			if err := searchChonk(sindex, dbstart, end, fidStr, settings, cmap, idmap, meta, db); err != nil {
				once.Do(func() {
					firstErr = fmt.Errorf("searchChonk at offset %d for file %q: %w", sindex, fidStr, err)
					cancel()
				})
			}
		}(sindex)
	}

	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	return nil
}

func searchChonk(sindex, dbstart, end int64, fid string, settings searchSettings, cmap *structs.SeenChonkMap, idmap *structs.SearchIDMap, meta structs.FileMeta, db *badger.DB) error {
	s1key, state1, err := getChonkState(sindex, dbstart, end, meta, db)
	if err != nil {
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		return fmt.Errorf("getChonkState at sindex=%d for fid=%q: %w", sindex, fid, err)
	}

	count := cmap.GetOrCompute(s1key, func() int { return subBytesChonk(settings.queryBytes, state1) })
	if count > 0 {
		idmap.Set(fid, count)
	}

	nxtidx := sindex + cnst.ChonkSize
	if nxtidx >= end {
		return nil
	}

	_, state2, err := getChonkState(nxtidx, dbstart, end, meta, db)
	if err != nil {
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		return fmt.Errorf("getChonkState at nxtidx=%d for fid=%q: %w", nxtidx, fid, err)
	}

	qoffset := (len(state1) - 1) - (settings.queryLen - 2)
	qstate1 := state1[qoffset:]
	state2 = state2[:settings.queryLen-2]

	qstate := make([]byte, len(qstate1)+len(state2))
	copy(qstate, qstate1)
	copy(qstate[len(qstate1):], state2)

	count = subBytesChonk(settings.queryBytes, qstate)
	if count > 0 {
		idmap.Set(fid, count)
	}
	return nil
}

func getChonkState(searchIndex, dbstart, end int64, meta structs.FileMeta, db *badger.DB) ([]byte, []byte, error) {
	relKey := util.AppendToBytesSlice(cnst.RelationNamespace, meta.EviHash, cnst.DataSeperator, searchIndex)
	chash, err := dbio.GetNode(relKey, db)
	if err != nil {
		return nil, nil, err
	}
	ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
	state, err := dbio.GetChonkData(searchIndex, meta.Start, meta.Size, dbstart, end, ckey, db)
	return ckey, state, err
}

func subBytesChonk(query, chonk []byte) int {
	if isASCII(query) {
		return countASCIIFold(query, chonk)
	}
	return bytes.Count(bytes.ToLower(chonk), query)
}

func countASCIIFold(query, chonk []byte) int {
	if len(query) == 0 || len(query) > len(chonk) {
		return 0
	}

	count := 0
	lastStart := len(chonk) - len(query)
	for start := 0; start <= lastStart; {
		if asciiFoldMatchAt(query, chonk, start) {
			count++
			start += len(query)
			continue
		}
		start++
	}

	return count
}

func asciiFoldMatchAt(query, chonk []byte, start int) bool {
	for index := 0; index < len(query); index++ {
		if toLowerASCII(query[index]) != toLowerASCII(chonk[start+index]) {
			return false
		}
	}
	return true
}

func isASCII(data []byte) bool {
	for _, value := range data {
		if value > 0x7f {
			return false
		}
	}
	return true
}

func toLowerASCII(value byte) byte {
	if value >= 'A' && value <= 'Z' {
		return value + ('a' - 'A')
	}
	return value
}

func searchReport(reportPath, query string, ranked []RankedResult, db *badger.DB) error {
	var report structs.SearchReport
	report.SchemaVersion = structs.SearchReportSchemaVersion
	report.Query = query
	seenMap := make(map[string][]string)

	var occurrenceCount, fileCount int
	artefactCount := len(ranked)

	for _, rr := range ranked {
		names, err := near.GetNames([]byte(rr.FileID), db)
		if err != nil {
			if errors.Is(err, badger.ErrKeyNotFound) {
				names = make(map[string]struct{})
			} else {
				return fmt.Errorf("GetNames for %q: %w", rr.FileID, err)
			}
		}

		hashStr, err := artefactHashFromSearchID(rr.FileID)
		if err != nil {
			return fmt.Errorf("processing ranked result: %w", err)
		}

		var occurrence structs.OccurrenceData
		occurrence.ArtefactHash = hashStr
		occurrence.Count = rr.TF
		occurrence.BM25Score = rr.Score
		occurrence.Disk = structs.NewDiskImage()
		err = setOccurrenceData(occurrence.ArtefactHash, names, seenMap, &occurrence, db)
		if err != nil {
			return err
		}

		if occurrence.Disk.Partition.Indexed != nil && len(occurrence.Disk.Partition.Indexed.IndexedFileNames) == 0 {
			occurrence.Disk.Partition.Indexed = nil
		}
		if occurrence.Disk.Partition != nil && len(occurrence.Disk.Partition.PartitionPartNames) == 0 {
			occurrence.Disk.Partition = nil
		}
		if occurrence.Disk != nil && len(occurrence.Disk.DiskImageNames) == 0 {
			occurrence.Disk = nil
		}

		fileCount += len(names)
		occurrenceCount += rr.TF
		report.Occurrences = append(report.Occurrences, occurrence)
	}

	report.ExecutiveSummary = fmt.Sprintf(
		"%d occurrences of the key term '%s' were identified across %d digital artefacts (%d files) during a comprehensive electronic examination. This finding presents avenues for further forensic analysis and legal evaluation.",
		occurrenceCount, query, artefactCount, fileCount,
	)

	reportData, err := json.MarshalIndent(report, "", "\t")
	if err != nil {
		return err
	}

	return os.WriteFile(reportPath, reportData, 0o644)
}

func artefactHashFromSearchID(id string) (string, error) {
	parts := strings.SplitN(id, cnst.NamespaceSeperator, 2)
	if len(parts) != 2 || parts[1] == "" {
		return "", fmt.Errorf("malformed search id: %q", id)
	}

	return base64.StdEncoding.EncodeToString([]byte(parts[1])), nil
}

func setOccurrenceData(artefactHash string, names map[string]struct{}, smap map[string][]string, occurrence *structs.OccurrenceData, db *badger.DB) error {
	var hierarchyIdx int
	for name := range names {
		split := strings.Split(name, cnst.DataSeperator)
		splitLen := len(split)

		switch splitLen {
		case 1:
			occurrence.FileNames = append(occurrence.FileNames, name)
			continue
		case 2:
			occurrence.Disk.Partition.Indexed.IndexedFileHash = occurrence.ArtefactHash
		case 3:
			occurrence.Disk.Partition.PartitionHash = occurrence.ArtefactHash
		default:
			return fmt.Errorf(cnst.ErrTooManySplits.Error(), name)
		}

		sameOccurrence := hierarchyIdx > 0
		err := setDiskImageData(sameOccurrence, artefactHash, split, smap, occurrence, db)
		if err != nil {
			return err
		}

		hierarchyIdx++
	}

	return nil
}

func setDiskImageData(same bool, artefactHash string, nameSplit []string, smap map[string][]string, occurrence *structs.OccurrenceData, db *badger.DB) error {
	var err error

	splitLen := len(nameSplit)
	name := nameSplit[splitLen-1]

	switch splitLen {
	case 3:
		occurrence.Disk.Partition.Indexed.IndexedFileHash = artefactHash
		occurrence.Disk.Partition.Indexed.IndexedFileNames = append(occurrence.Disk.Partition.Indexed.IndexedFileNames, name)

		partitionHash := nameSplit[1]
		occurrence.Disk.Partition.PartitionHash = partitionHash
		partitionNames, ok := smap[cnst.PartiFileNamespace+partitionHash]
		if ok {
			if !same || len(occurrence.Disk.Partition.PartitionPartNames) == 0 {
				occurrence.Disk.Partition.PartitionPartNames = partitionNames
			}
		} else {
			occurrence.Disk.Partition.PartitionPartNames, err = getFileNames(cnst.PartiFileNamespace, partitionHash, db)
			if err != nil {
				return err
			}
			smap[cnst.PartiFileNamespace+partitionHash] = occurrence.Disk.Partition.PartitionPartNames
		}

		diskHash := nameSplit[0]
		occurrence.Disk.DiskImageHash = diskHash
		diskNames, ok := smap[cnst.EviFileNamespace+diskHash]
		if ok {
			if !same || len(occurrence.Disk.DiskImageNames) == 0 {
				occurrence.Disk.DiskImageNames = diskNames
			}
		} else {
			occurrence.Disk.DiskImageNames, err = getFileNames(cnst.EviFileNamespace, diskHash, db)
			if err != nil {
				return err
			}
			smap[cnst.EviFileNamespace+diskHash] = occurrence.Disk.DiskImageNames
		}
	case 2:
		occurrence.Disk.Partition.PartitionHash = artefactHash
		occurrence.Disk.Partition.PartitionPartNames = append(occurrence.Disk.Partition.PartitionPartNames, name)

		diskHash := nameSplit[0]
		occurrence.Disk.DiskImageHash = diskHash
		diskNames, ok := smap[cnst.EviFileNamespace+diskHash]
		if ok {
			if !same || len(occurrence.Disk.DiskImageNames) == 0 {
				occurrence.Disk.DiskImageNames = diskNames
			}
		} else {
			occurrence.Disk.DiskImageNames, err = getFileNames(cnst.EviFileNamespace, diskHash, db)
			if err != nil {
				return err
			}
			smap[cnst.EviFileNamespace+diskHash] = occurrence.Disk.DiskImageNames
		}
	}

	return err
}

func buildKeywordCountMap(results map[string]int) map[string]int64 {
	keywordCountMap := make(map[string]int64, len(results))
	for fileID, count := range results {
		encodedID := base64.StdEncoding.EncodeToString([]byte(fileID))
		keywordCountMap[encodedID] = int64(count)
	}
	return keywordCountMap
}

func countTotalOccurrences(results map[string]int) int64 {
	var totalCount int64
	for _, count := range results {
		totalCount += int64(count)
	}
	return totalCount
}

func getFileNames(namespace, encodedHash string, db *badger.DB) ([]string, error) {
	hash, err := base64.StdEncoding.DecodeString(encodedHash)
	if err != nil {
		return nil, err
	}

	fid := util.AppendToBytesSlice(namespace, hash)
	nameMap, err := near.GetNames(fid, db)
	if err != nil {
		return nil, err
	}

	var names []string
	for name := range nameMap {
		split := strings.Split(name, cnst.DataSeperator)
		names = append(names, split[len(split)-1])
	}

	return names, nil
}
