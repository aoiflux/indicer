package near

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/fatih/color"
	"github.com/schollz/progressbar/v3"
	"github.com/vmihailenco/msgpack/v5"
)

const minDeepPartialConfidence = 0.75
const shallowExactRankWeight = 1.35
const deepSimhashRankWeight = 1.00

type nearJSONReport struct {
	GeneratedAt       string            `json:"generated_at"`
	DurationMs        int64             `json:"duration_ms"`
	DeepMode          bool              `json:"deep_mode"`
	VerifyMode        bool              `json:"advanced_deep_mode"`
	VerifyTopK        int               `json:"verify_top_k,omitempty"`
	VerifyDurationMs  int64             `json:"verify_duration_ms,omitempty"`
	SimilarityWarning string            `json:"similarity_warning,omitempty"`
	Input             nearReportInput   `json:"input"`
	Summary           nearReportSummary `json:"summary"`
	Matches           []nearReportMatch `json:"matches"`
}

type nearReportInput struct {
	QueryHashBase64 string   `json:"query_hash_base64"`
	QueryID         string   `json:"query_id"`
	QueryType       string   `json:"query_type"`
	QueryNames      []string `json:"query_names,omitempty"`
	ExactFileMatch  bool     `json:"exact_file_match,omitempty"`
	ExplainExact    bool     `json:"explain_exact,omitempty"`
}

type nearReportSummary struct {
	TotalMatches int `json:"total_matches"`
	Evidence     int `json:"evidence_matches"`
	Partition    int `json:"partition_matches"`
	Indexed      int `json:"indexed_matches"`
}

type nearReportMatch struct {
	Rank              int      `json:"rank"`
	ID                string   `json:"id"`
	Type              string   `json:"type"`
	ExactMatch        bool     `json:"exact_match"`
	MatchMethod       string   `json:"match_method,omitempty"`
	HashBase64        string   `json:"hash_base64"`
	Names             []string `json:"names,omitempty"`
	Start             int64    `json:"start"`
	End               int64    `json:"end"`
	Size              int64    `json:"size"`
	Confidence        float64  `json:"confidence"`
	ConfidencePercent float64  `json:"confidence_percent"`
	// OverallRelatedness is the single synthesised score that represents how strongly
	// the two artefacts are related, taking into account every phase that was executed.
	// See RelatednessBasis for how the value was derived.
	OverallRelatedness    float64 `json:"overall_relatedness"`
	OverallRelatednessPct float64 `json:"overall_relatedness_percent"`
	// RelatednessBasis documents which signals were blended: "exact", "phase1", "phase1+phase2".
	RelatednessBasis           string           `json:"relatedness_basis"`
	InternalObjectSize         int              `json:"internal_object_size"`
	ChunkMatchCount            int              `json:"chunk_match_count"`
	DeepMatchCount             int              `json:"deep_match_count"`
	ShallowMatchCount          int              `json:"shallow_match_count"`
	ChunkConfidenceSum         float64          `json:"chunk_confidence_sum"`
	ChunkConfidenceAvg         float64          `json:"chunk_confidence_avg"`
	WeightedChunkConfidenceSum float64          `json:"weighted_chunk_confidence_sum"`
	WeightedChunkConfidenceAvg float64          `json:"weighted_chunk_confidence_avg"`
	MatchingChunks             []nearChunkMatch `json:"matching_chunks,omitempty"`
	// Phase 1 alignment noise estimate (0.0 = fully aligned, 1.0 = all chunks are edge chunks).
	Phase1DeviationEstimate float64 `json:"phase1_deviation_estimate"`
	Phase1DeviationWarning  string  `json:"phase1_deviation_warning,omitempty"`
	// Phase 2 full-file SimHash re-ranking fields (populated when --verify is used).
	Phase2FileSimilarity    float64 `json:"phase2_file_similarity,omitempty"`
	Phase2FileSimilarityPct float64 `json:"phase2_file_similarity_pct,omitempty"`
	Phase2Rank              int     `json:"phase2_rank,omitempty"`
}

type nearChunkMatch struct {
	Index             int64   `json:"index"`
	Method            string  `json:"method"`
	Confidence        float64 `json:"confidence"`
	ConfidencePercent float64 `json:"confidence_percent"`
}

type nearChunkContribution struct {
	Index      int64
	Confidence float64
	Method     string
}

var nearChunkContribStore = struct {
	mu   sync.Mutex
	data map[string][]nearChunkContribution
}{
	data: make(map[string][]nearChunkContribution),
}

var nearExactMatchStore = struct {
	mu   sync.Mutex
	data map[string]struct{}
}{
	data: make(map[string]struct{}),
}

func resetNearChunkContributions() {
	nearChunkContribStore.mu.Lock()
	defer nearChunkContribStore.mu.Unlock()
	nearChunkContribStore.data = make(map[string][]nearChunkContribution)

	nearExactMatchStore.mu.Lock()
	defer nearExactMatchStore.mu.Unlock()
	nearExactMatchStore.data = make(map[string]struct{})
}

func addNearChunkContribution(id string, contribution nearChunkContribution) {
	nearChunkContribStore.mu.Lock()
	defer nearChunkContribStore.mu.Unlock()
	nearChunkContribStore.data[id] = append(nearChunkContribStore.data[id], contribution)
}

func getNearChunkContributions(id string) []nearChunkContribution {
	nearChunkContribStore.mu.Lock()
	defer nearChunkContribStore.mu.Unlock()
	values := nearChunkContribStore.data[id]
	out := make([]nearChunkContribution, len(values))
	copy(out, values)
	return out
}

func addNearExactMatchID(id string) {
	nearExactMatchStore.mu.Lock()
	defer nearExactMatchStore.mu.Unlock()
	nearExactMatchStore.data[id] = struct{}{}
}

func isNearExactMatchID(id string) bool {
	nearExactMatchStore.mu.Lock()
	defer nearExactMatchStore.mu.Unlock()
	_, ok := nearExactMatchStore.data[id]
	return ok
}

func NearInFile(fhash string, db *badger.DB, deep, verify bool, topK int) error {
	fmt.Println("Finding NeAR artefacts & generating Artefact Relation Graph")
	start := time.Now()
	resetNearChunkContributions()

	fid, err := dbio.GuessFileType(fhash, db)
	if err != nil {
		return err
	}
	addNearExactMatchID(string(fid))

	// Phase 2: obtain full-file SimHash of the query file.
	// Cache-first: if a previous --advanced-deep run already stored it, reuse it.
	// Otherwise stream the stored chunk bytes, compute, and store for future runs.
	var vcfg verifyConfig
	if verify {
		var querySig uint64
		var sigOK bool

		if cached, cacheErr := dbio.GetFileSimhash(fid, db); cacheErr == nil {
			querySig = cached
			sigOK = true
		} else {
			acc := newFileSimHashAccumulator()
			if streamErr := dbio.StreamLogicalFileBytes(fid, db, func(chunk []byte) error {
				acc.write(chunk)
				return nil
			}); streamErr == nil {
				querySig = acc.finalize()
				sigOK = true
				_ = dbio.SetFileSimhash(fid, querySig, db)
			}
		}

		if sigOK {
			vcfg = verifyConfig{enabled: true, topK: topK, querySig: querySig}
		}
		// If obtaining the query sig fails entirely, verify is silently disabled.
	}

	var idmap *structs.ConcMap

	if deep {
		color.Red("DEEP option selected. NeAR calculation may take a long time.")
	}

	if bytes.HasPrefix(fid, []byte(cnst.IdxFileNamespace)) {
		idmap, err = nearIndexFile(fid, db, deep)
	} else if bytes.HasPrefix(fid, []byte(cnst.PartiFileNamespace)) {
		idmap, err = nearPartitionFile(fid, db, deep)
	} else {
		idmap, err = nearEvidenceFile(fid, db, deep)
	}
	if err != nil {
		return err
	}

	err = updateConfidence(idmap, db)
	if err != nil {
		return err
	}
	idmap.Set(string(fid), 100, true)

	reportPath, err := writeNearJSONReport(fid, fhash, idmap, deep, time.Since(start), db, vcfg)
	if err != nil {
		return err
	}
	fmt.Printf("NeAR report generated: %s\n", reportPath)

	// err = visualise(fid, idmap, db)
	// if err != nil {
	// 	return err
	// }

	fmt.Printf("Done.... %v\n", time.Since(start))
	return nil
}

func writeNearJSONReport(fid []byte, queryHash string, idmap *structs.ConcMap, deep bool, runtime time.Duration, db *badger.DB, vcfg verifyConfig) (string, error) {
	queryNames, err := getNearInputNames(fid, db)
	if err != nil {
		return "", err
	}
	input := nearReportInput{
		QueryHashBase64: queryHash,
		QueryID:         base64.StdEncoding.EncodeToString(fid),
		QueryType:       nearFileType(fid),
		QueryNames:      queryNames,
	}

	return writeNearJSONReportWithInput(input, idmap, deep, runtime, db, vcfg)
}

func writeNearJSONReportWithInput(input nearReportInput, idmap *structs.ConcMap, deep bool, runtime time.Duration, db *badger.DB, vcfg verifyConfig) (string, error) {
	report := nearJSONReport{
		GeneratedAt: time.Now().Format(time.RFC3339),
		DurationMs:  runtime.Milliseconds(),
		DeepMode:    deep,
		VerifyMode:  vcfg.enabled,
		Input:       input,
		Matches:     make([]nearReportMatch, 0),
	}

	// Set a top-level similarity warning so consumers understand Phase 1 limitations.
	if vcfg.isOutfile && deep {
		report.SimilarityWarning = "Phase 1 deep-simhash scores compare fixed 256 KB block windows. " +
			"The query file's chunk grid is unlikely to align with stored files' chunk grids " +
			"(fixed-offset chunking), so confidence values are approximate. " +
			"Use --verify for full-file SimHash re-ranking which is alignment-independent."
	} else if deep {
		report.SimilarityWarning = "Deep-simhash confidence values may be affected by partial " +
			"edge-chunk alignment (see phase1_deviation_estimate per match). " +
			"Use --verify to apply full-file SimHash re-ranking and reduce this noise."
	}

	for id, confidence := range idmap.GetData() {
		match, err := buildNearReportMatch([]byte(id), confidence, db)
		if err != nil {
			return "", err
		}
		report.Matches = append(report.Matches, match)
	}

	sort.Slice(report.Matches, func(i, j int) bool {
		if report.Matches[i].ExactMatch != report.Matches[j].ExactMatch {
			return report.Matches[i].ExactMatch
		}
		if report.Matches[i].WeightedChunkConfidenceSum != report.Matches[j].WeightedChunkConfidenceSum {
			return report.Matches[i].WeightedChunkConfidenceSum > report.Matches[j].WeightedChunkConfidenceSum
		}
		if report.Matches[i].ChunkMatchCount != report.Matches[j].ChunkMatchCount {
			return report.Matches[i].ChunkMatchCount > report.Matches[j].ChunkMatchCount
		}
		if report.Matches[i].WeightedChunkConfidenceAvg != report.Matches[j].WeightedChunkConfidenceAvg {
			return report.Matches[i].WeightedChunkConfidenceAvg > report.Matches[j].WeightedChunkConfidenceAvg
		}
		if report.Matches[i].ChunkConfidenceAvg != report.Matches[j].ChunkConfidenceAvg {
			return report.Matches[i].ChunkConfidenceAvg > report.Matches[j].ChunkConfidenceAvg
		}
		if report.Matches[i].ChunkConfidenceSum != report.Matches[j].ChunkConfidenceSum {
			return report.Matches[i].ChunkConfidenceSum > report.Matches[j].ChunkConfidenceSum
		}
		if report.Matches[i].Confidence != report.Matches[j].Confidence {
			return report.Matches[i].Confidence > report.Matches[j].Confidence
		}
		return report.Matches[i].Size > report.Matches[j].Size
	})

	for i := range report.Matches {
		report.Matches[i].Rank = i + 1
		report.Summary.TotalMatches++
		switch report.Matches[i].Type {
		case "evidence":
			report.Summary.Evidence++
		case "partition":
			report.Summary.Partition++
		case "indexed":
			report.Summary.Indexed++
		}
	}

	// Phase 2: full-file SimHash re-ranking of top K candidates.
	if vcfg.enabled && len(report.Matches) > 0 {
		verifyStart := time.Now()
		k := vcfg.topK
		if k <= 0 {
			// Auto-select K and tell the user what drove the decision.
			var freeGB float64
			var threads int
			k, freeGB, threads = computeDynamicTopKWithReason(len(report.Matches))
			fmt.Printf("[advanced-deep] auto-selected top-K = %d "+
				"(%.1f GB available memory × %d CPU threads, bounded [5, 100], capped at %d total matches)\n",
				k, freeGB, threads, len(report.Matches))
		}
		if k > len(report.Matches) {
			k = len(report.Matches)
		}
		report.VerifyTopK = k
		// topK is a live slice into report.Matches; verifyPhase2 re-sorts it in-place.
		if err := verifyPhase2(vcfg.querySig, report.Matches[:k], db); err != nil {
			return "", err
		}
		report.VerifyDurationMs = time.Since(verifyStart).Milliseconds()
	}

	// Compute the unified overall_relatedness for every match, then do a final
	// re-sort by that score and re-assign Rank so the report reflects the best
	// combined signal (Phase 1, or Phase 1 + Phase 2, or exact).
	for i := range report.Matches {
		computeOverallRelatedness(&report.Matches[i])
	}
	sort.SliceStable(report.Matches, func(i, j int) bool {
		return report.Matches[i].OverallRelatedness > report.Matches[j].OverallRelatedness
	})
	for i := range report.Matches {
		report.Matches[i].Rank = i + 1
	}

	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", err
	}

	fname := "near_report.json"
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	reportPath := filepath.Join(cwd, fname)

	err = os.WriteFile(reportPath, body, 0o644)
	if err != nil {
		return "", err
	}

	return reportPath, nil
}

func getNearInputNames(fid []byte, db *badger.DB) ([]string, error) {
	switch {
	case bytes.HasPrefix(fid, []byte(cnst.IdxFileNamespace)):
		ifile, err := dbio.GetIndexedFile(fid, db)
		if err != nil {
			return nil, err
		}
		return mapKeysSorted(ifile.Names), nil
	case bytes.HasPrefix(fid, []byte(cnst.PartiFileNamespace)):
		pfile, err := dbio.GetPartitionFile(fid, db)
		if err != nil {
			return nil, err
		}
		return mapKeysSorted(pfile.Names), nil
	case bytes.HasPrefix(fid, []byte(cnst.EviFileNamespace)):
		efile, err := dbio.GetEvidenceFile(fid, db)
		if err != nil {
			return nil, err
		}
		return mapKeysSorted(efile.Names), nil
	default:
		return nil, nil
	}
}

// computeOverallRelatedness synthesises a single relatedness score from all
// phases that ran for this match and writes it into the match in-place.
//
// Formula:
//   - exact-file-hash  : 1.0  (identity — no blend needed)
//   - Phase 2 ran (Phase2Rank > 0): 0.60 × phase2_file_similarity + 0.40 × confidence
//     Phase 2 is alignment-independent and therefore the dominant signal; Phase 1
//     confidence captures chunk-level coverage and provides the remaining weight.
//   - Phase 1 only     : confidence  (best available signal)
//
// RelatednessBasis records which path was taken so consumers can interpret the value.
func computeOverallRelatedness(m *nearReportMatch) {
	switch {
	case m.MatchMethod == "exact-file-hash":
		m.OverallRelatedness = 1.0
		m.OverallRelatednessPct = 100.0
		m.RelatednessBasis = "exact"
	case m.Phase2Rank > 0:
		// Phase 2 ran for this candidate: blend phase2 (60%) and phase1 (40%).
		m.OverallRelatedness = 0.60*m.Phase2FileSimilarity + 0.40*m.Confidence
		m.OverallRelatednessPct = m.OverallRelatedness * 100
		m.RelatednessBasis = "phase1+phase2"
	default:
		m.OverallRelatedness = m.Confidence
		m.OverallRelatednessPct = m.Confidence * 100
		m.RelatednessBasis = "phase1"
	}
}

func buildNearReportMatch(id []byte, confidence float64, db *badger.DB) (nearReportMatch, error) {
	match := nearReportMatch{
		ID:                base64.StdEncoding.EncodeToString(id),
		Type:              nearFileType(id),
		HashBase64:        getHashFromID(id),
		Confidence:        confidence / 100,
		ConfidencePercent: confidence,
	}

	if isNearExactMatchID(string(id)) {
		match.ExactMatch = true
		match.MatchMethod = "exact-file-hash"
	}

	chunkContrib := getNearChunkContributions(string(id))
	sort.Slice(chunkContrib, func(i, j int) bool {
		if chunkContrib[i].Index == chunkContrib[j].Index {
			return chunkContrib[i].Method < chunkContrib[j].Method
		}
		return chunkContrib[i].Index < chunkContrib[j].Index
	})

	match.MatchingChunks = make([]nearChunkMatch, 0, len(chunkContrib))
	for _, c := range chunkContrib {
		entry := nearChunkMatch{
			Index:             c.Index,
			Method:            c.Method,
			Confidence:        c.Confidence,
			ConfidencePercent: c.Confidence * 100,
		}
		match.MatchingChunks = append(match.MatchingChunks, entry)
		match.ChunkConfidenceSum += c.Confidence
		match.WeightedChunkConfidenceSum += c.Confidence * getChunkMethodRankWeight(c.Method)
		if strings.HasPrefix(c.Method, "deep") {
			match.DeepMatchCount++
		} else {
			match.ShallowMatchCount++
		}
		if match.MatchMethod == "" {
			match.MatchMethod = c.Method
		}
	}
	match.ChunkMatchCount = len(match.MatchingChunks)
	if match.ChunkMatchCount > 0 {
		match.ChunkConfidenceAvg = match.ChunkConfidenceSum / float64(match.ChunkMatchCount)
		match.WeightedChunkConfidenceAvg = match.WeightedChunkConfidenceSum / float64(match.ChunkMatchCount)
	}

	switch {
	case bytes.HasPrefix(id, []byte(cnst.IdxFileNamespace)):
		ifile, err := dbio.GetIndexedFile(id, db)
		if err != nil {
			return match, err
		}
		match.Names = mapKeysSorted(ifile.Names)
		match.Start = ifile.Start
		match.Size = ifile.Size
		match.End = ifile.Start + ifile.Size
	case bytes.HasPrefix(id, []byte(cnst.PartiFileNamespace)):
		pfile, err := dbio.GetPartitionFile(id, db)
		if err != nil {
			return match, err
		}
		match.Names = mapKeysSorted(pfile.Names)
		match.Start = pfile.Start
		match.Size = pfile.Size
		match.End = pfile.Start + pfile.Size
		match.InternalObjectSize = len(pfile.InternalObjects)
	case bytes.HasPrefix(id, []byte(cnst.EviFileNamespace)):
		efile, err := dbio.GetEvidenceFile(id, db)
		if err != nil {
			return match, err
		}
		match.Names = mapKeysSorted(efile.Names)
		match.Start = efile.Start
		match.Size = efile.Size
		match.End = efile.Start + efile.Size
		match.InternalObjectSize = len(efile.InternalObjects)
	}

	// Exact file hash matches have no chunk-alignment noise by definition —
	// the file was found via direct hash equality, not chunk simhash comparison.
	// Skip deviation so it stays at its zero default rather than showing a
	// misleading value.
	if match.MatchMethod != "exact-file-hash" {
		_, match.Phase1DeviationEstimate, match.Phase1DeviationWarning = computeAlignmentDeviation(match.Start, match.Size)
	}

	return match, nil
}

func mapKeysSorted(kv map[string]struct{}) []string {
	out := make([]string, 0, len(kv))
	for key := range kv {
		split := strings.Split(key, cnst.DataSeperator)
		key = split[len(split)-1]
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func getHashFromID(id []byte) string {
	split := bytes.SplitN(id, []byte(cnst.NamespaceSeperator), 2)
	if len(split) != 2 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(split[1])
}

func getChunkMethodRankWeight(method string) float64 {
	switch method {
	case "shallow-exact":
		return shallowExactRankWeight
	case "deep-simhash":
		return deepSimhashRankWeight
	default:
		return 1
	}
}

func nearFileType(id []byte) string {
	switch {
	case bytes.HasPrefix(id, []byte(cnst.EviFileNamespace)):
		return "evidence"
	case bytes.HasPrefix(id, []byte(cnst.PartiFileNamespace)):
		return "partition"
	case bytes.HasPrefix(id, []byte(cnst.IdxFileNamespace)):
		return "indexed"
	default:
		return "unknown"
	}
}

func nearIndexFile(fid []byte, db *badger.DB, deep ...bool) (*structs.ConcMap, error) {
	ifile, err := dbio.GetIndexedFile(fid, db)
	if err != nil {
		return nil, err
	}
	var isdeep bool
	if len(deep) > 0 {
		isdeep = deep[0]
	}
	iname := util.GetArbitratyMapKey(ifile.Names)
	return getNearLogicalFile(ifile.Start, ifile.Size, iname, fid, db, isdeep)
}
func nearPartitionFile(fid []byte, db *badger.DB, deep ...bool) (*structs.ConcMap, error) {
	pfile, err := dbio.GetPartitionFile(fid, db)
	if err != nil {
		return nil, err
	}
	var isdeep bool
	if len(deep) > 0 {
		isdeep = deep[0]
	}
	pname := util.GetArbitratyMapKey(pfile.Names)
	return getNearLogicalFile(pfile.Start, pfile.Size, pname, fid, db, isdeep)
}
func nearEvidenceFile(fid []byte, db *badger.DB, deep ...bool) (*structs.ConcMap, error) {
	efile, err := dbio.GetEvidenceFile(fid, db)
	if err != nil {
		return nil, err
	}
	ehash := bytes.Split(fid, []byte(cnst.NamespaceSeperator))[1]
	var isdeep bool
	if len(deep) > 0 {
		isdeep = deep[0]
	}
	return getNearFile(efile.Start, efile.Size, ehash, fid, db, isdeep)
}

func getNearLogicalFile(start, size int64, fname string, fid []byte, db *badger.DB, deep ...bool) (*structs.ConcMap, error) {
	ehash, err := util.GetEvidenceFileHash(fname)
	if err != nil {
		return nil, err
	}
	var isdeep bool
	if len(deep) > 0 {
		isdeep = deep[0]
	}
	return getNearFile(start, size, ehash, fid, db, isdeep)
}
func getNearFile(start, size int64, ehash, fid []byte, db *badger.DB, deep ...bool) (*structs.ConcMap, error) {
	fhash := bytes.Split(fid, []byte(cnst.NamespaceSeperator))[1]
	idmap := structs.NewConcMap()

	fmt.Println("Finding NeAR Artefacts....")
	bar := progressbar.NewOptions64(
		size,
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "#",
			SaucerHead:    ">",
			SaucerPadding: "-",
			BarStart:      "[",
			BarEnd:        "]",
		}),
	)

	var active int
	echan := make(chan error)

	var isdeep bool
	if len(deep) > 0 {
		isdeep = deep[0]
	}

	for near := range getNear(start, size, ehash, db, isdeep) {
		if active >= cnst.GetMaxThreadCount() {
			err := <-echan
			if err != nil {
				return nil, err
			}
			bar.Add64(cnst.ChonkSize)
			active--
		}

		if near.Err != nil {
			return nil, near.Err
		}
		if len(near.RevMap) == 0 {
			continue
		}

		go countRList(fhash, idmap, near, db, echan)
		active++
	}

	for active > 0 {
		err := <-echan
		if err != nil {
			return nil, err
		}
		bar.Add64(cnst.ChonkSize)
		active--
	}

	bar.Finish()
	fmt.Println("Found NeAR Artefacts. Generating Artefact Relation Graph....")
	return idmap, bar.Close()
}

func updateConfidence(idmap *structs.ConcMap, db *badger.DB) error {
	var size int64
	for id := range idmap.GetData() {
		if strings.HasPrefix(id, cnst.IdxFileNamespace) {
			ifile, err := dbio.GetIndexedFile([]byte(id), db)
			if err != nil {
				return err
			}
			size = ifile.Size
		}
		if strings.HasPrefix(id, cnst.PartiFileNamespace) {
			pfile, err := dbio.GetPartitionFile([]byte(id), db)
			if err != nil {
				return err
			}
			size = pfile.Size
		}
		if strings.HasPrefix(id, cnst.EviFileNamespace) {
			efile, err := dbio.GetEvidenceFile([]byte(id), db)
			if err != nil {
				return err
			}
			size = efile.Size
		}

		chonks := float64((size + cnst.ChonkSize - 1) / cnst.ChonkSize)
		if chonks <= 0 {
			continue
		}
		confidence, _ := idmap.Get(id)
		confidence = (confidence / chonks) * 100
		idmap.Set(id, confidence, true)
	}

	return nil
}

// getNear function loops through entire file indexed in db
// finds all the relation nodes, uses relation nodes to find
// all the chonk --> rel reverse relation objects
func getNear(start, size int64, ehash []byte, db *badger.DB, deep bool) chan structs.NearGen {
	neargenChan := make(chan structs.NearGen)
	seenMap := make(map[int64]struct{})

	var dbstart int64
	if start > 0 {
		dbstart = util.GetDBStartOffset(start)
	}

	go func() {
		defer close(neargenChan)

		end := start + size
		var neargen structs.NearGen

		var confidence float64
		for nearIndex := dbstart; nearIndex < end; nearIndex += cnst.ChonkSize {
			relKey := util.AppendToBytesSlice(cnst.RelationNamespace, ehash, cnst.DataSeperator, nearIndex)
			split := bytes.Split(relKey, []byte(cnst.DataSeperator))
			idxstr := split[len(split)-1]
			idx, err := util.GetNumber(string(idxstr))
			if err != nil {
				neargen.Err = err
				neargenChan <- neargen
				return
			}
			if _, ok := seenMap[idx]; ok {
				continue
			} else {
				seenMap[idx] = struct{}{}
			}

			chash, err := dbio.GetNode(relKey, db)
			if err != nil {
				neargen.Err = err
				neargenChan <- neargen
				return
			}

			revkey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator, nearIndex)
			revmap, err := dbio.GetReverseRelationNode(revkey, db)
			if err != nil {
				neargen.Err = err
				neargenChan <- neargen
				return
			}
			if len(revmap) < 2 && !deep {
				continue
			}
			confidence = 1
			matchMethod := "shallow-exact"

			if len(revmap) < 2 && deep {
				revmap, confidence, err = partialMatch(ehash, chash, db)
				if err != nil {
					neargen.Err = err
					neargenChan <- neargen
					return
				}
				if len(revmap) == 0 {
					continue
				}
				matchMethod = "deep-simhash"
			}

			neargen.RevMap = make(map[int64][]string)
			for revid := range revmap {
				delete(revmap, revid)
				revlist, ok := neargen.RevMap[nearIndex]
				if !ok {
					neargen.RevMap[nearIndex] = []string{revid}
					continue
				}
				revlist = append(revlist, revid)
				neargen.RevMap[nearIndex] = revlist
			}

			similarMap, err := getRevRelSameChashPrefix(revkey, db)
			if err != nil {
				neargen.Err = err
				neargenChan <- neargen
				return
			}
			for tempIndex, tempRevList := range similarMap {
				if _, ok := seenMap[tempIndex]; ok {
					continue
				} else {
					seenMap[tempIndex] = struct{}{}
				}
				if found := util.FindInStringSlice(tempRevList, string(ehash)); found != int(cnst.IgnoreVar) {
					tempRevList = util.Reslice(tempRevList, found)
				}
				if len(tempRevList) < 1 {
					continue
				}
				if _, ok := neargen.RevMap[tempIndex]; !ok {
					neargen.RevMap[tempIndex] = tempRevList
				}
				delete(similarMap, tempIndex)
			}

			neargen.Confidence = confidence
			neargen.MatchMethod = matchMethod
			neargenChan <- neargen
		}
	}()

	return neargenChan
}

func partialMatch(inhash, chash []byte, db *badger.DB) (map[string]struct{}, float64, error) {
	inputSig, err := dbio.GetChonkSignature(chash, db)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, float64(cnst.IgnoreVar), nil
	}
	if err != nil {
		return nil, float64(cnst.IgnoreVar), err
	}

	partialMatchKey, confidence, err := partialChonkMatch(inhash, inputSig, db)
	if err != nil || confidence < minDeepPartialConfidence {
		return nil, float64(cnst.IgnoreVar), err
	}

	revmap, err := dbio.GetReverseRelationNode(partialMatchKey, db)
	if err != nil {
		return nil, float64(cnst.IgnoreVar), err
	}

	return revmap, confidence, nil
}

// getRevRelSameChashPrefix function will find all the relation keys with same chonk hash but different index
// Same chonk can occur at different index locations in different files, to improve accuracy, all indices
// from all files for the same chonk must be taken care of
func getRevRelSameChashPrefix(revkey []byte, db *badger.DB) (map[int64][]string, error) {
	split := bytes.Split(revkey, []byte(cnst.DataSeperator))
	prefix := util.AppendToBytesSlice(split[0], cnst.DataSeperator, split[1])
	similarMap := make(map[int64][]string)

	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 1000
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			revid := item.KeyCopy(nil)

			data, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}

			data, err = cnst.DECODER.DecodeAll(data, nil)
			if err != nil {
				return err
			}

			var tempRevMap map[string]struct{}
			err = msgpack.Unmarshal(data, &tempRevMap)
			if err != nil {
				return err
			}

			split := bytes.Split(revid, []byte(cnst.DataSeperator))
			idxstr := split[len(split)-1]
			idx, err := util.GetNumber(string(idxstr))
			if err != nil {
				return err
			}

			for revid := range tempRevMap {
				revlist, ok := similarMap[idx]
				if !ok {
					similarMap[idx] = []string{revid}
					continue
				}
				revlist = append(revlist, revid)
				similarMap[idx] = revlist
			}
		}

		return nil
	})

	return similarMap, err
}
