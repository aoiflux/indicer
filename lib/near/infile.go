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
	GeneratedAt string            `json:"generated_at"`
	DurationMs  int64             `json:"duration_ms"`
	DeepMode    bool              `json:"deep_mode"`
	Input       nearReportInput   `json:"input"`
	Summary     nearReportSummary `json:"summary"`
	Matches     []nearReportMatch `json:"matches"`
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
	Rank                       int              `json:"rank"`
	ID                         string           `json:"id"`
	Type                       string           `json:"type"`
	HashBase64                 string           `json:"hash_base64"`
	Names                      []string         `json:"names,omitempty"`
	Start                      int64            `json:"start"`
	End                        int64            `json:"end"`
	Size                       int64            `json:"size"`
	Confidence                 float64          `json:"confidence"`
	ConfidencePercent          float64          `json:"confidence_percent"`
	InternalObjectSize         int              `json:"internal_object_size"`
	ChunkMatchCount            int              `json:"chunk_match_count"`
	DeepMatchCount             int              `json:"deep_match_count"`
	ShallowMatchCount          int              `json:"shallow_match_count"`
	ChunkConfidenceSum         float64          `json:"chunk_confidence_sum"`
	ChunkConfidenceAvg         float64          `json:"chunk_confidence_avg"`
	WeightedChunkConfidenceSum float64          `json:"weighted_chunk_confidence_sum"`
	WeightedChunkConfidenceAvg float64          `json:"weighted_chunk_confidence_avg"`
	MatchingChunks             []nearChunkMatch `json:"matching_chunks,omitempty"`
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

func resetNearChunkContributions() {
	nearChunkContribStore.mu.Lock()
	defer nearChunkContribStore.mu.Unlock()
	nearChunkContribStore.data = make(map[string][]nearChunkContribution)
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

func NearInFile(fhash string, db *badger.DB, deep ...bool) error {
	fmt.Println("Finding NeAR artefacts & generating Artefact Relation Graph")
	start := time.Now()
	resetNearChunkContributions()

	fid, err := dbio.GuessFileType(fhash, db)
	if err != nil {
		return err
	}

	var idmap *structs.ConcMap

	var isdeep bool
	if len(deep) > 0 {
		isdeep = deep[0]
	}
	if isdeep {
		color.Red("DEEP option selected. NeAR calculation may take a long time.")
	}

	if bytes.HasPrefix(fid, []byte(cnst.IdxFileNamespace)) {
		idmap, err = nearIndexFile(fid, db, isdeep)
	} else if bytes.HasPrefix(fid, []byte(cnst.PartiFileNamespace)) {
		idmap, err = nearPartitionFile(fid, db, isdeep)
	} else {
		idmap, err = nearEvidenceFile(fid, db, isdeep)
	}
	if err != nil {
		return err
	}

	err = updateConfidence(idmap, db)
	if err != nil {
		return err
	}

	reportPath, err := writeNearJSONReport(fid, fhash, idmap, isdeep, time.Since(start), db)
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

func writeNearJSONReport(fid []byte, queryHash string, idmap *structs.ConcMap, deep bool, runtime time.Duration, db *badger.DB) (string, error) {
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

	return writeNearJSONReportWithInput(input, idmap, deep, runtime, db)
}

func writeNearJSONReportWithInput(input nearReportInput, idmap *structs.ConcMap, deep bool, runtime time.Duration, db *badger.DB) (string, error) {
	report := nearJSONReport{
		GeneratedAt: time.Now().Format(time.RFC3339),
		DurationMs:  runtime.Milliseconds(),
		DeepMode:    deep,
		Input:       input,
		Matches:     make([]nearReportMatch, 0),
	}

	for id, confidence := range idmap.GetData() {
		match, err := buildNearReportMatch([]byte(id), confidence, db)
		if err != nil {
			return "", err
		}
		report.Matches = append(report.Matches, match)
	}

	sort.Slice(report.Matches, func(i, j int) bool {
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

func buildNearReportMatch(id []byte, confidence float64, db *badger.DB) (nearReportMatch, error) {
	match := nearReportMatch{
		ID:                base64.StdEncoding.EncodeToString(id),
		Type:              nearFileType(id),
		HashBase64:        getHashFromID(id),
		Confidence:        confidence / 100,
		ConfidencePercent: confidence,
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
	bar := progressbar.DefaultBytes(size)

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
