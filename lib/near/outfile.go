package near

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"
	"path/filepath"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/edsrzf/mmap-go"
)

func NearOutFile(fpath string, db *badger.DB, deep ...bool) error {
	start := time.Now()
	resetNearChunkContributions()

	size, fhash, mappedFile, err := outfileSetup(fpath)
	if err != nil {
		return err
	}
	defer mappedFile.Unmap()

	idmap := structs.NewConcMap()
	deepEnabled := false
	if len(deep) > 0 {
		deepEnabled = deep[0]
	}
	explainExact := false
	if len(deep) > 1 {
		explainExact = deep[1]
	}

	exactID, hasExact, err := getExactOutMatchID(fhash, db)
	if err != nil {
		return err
	}
	if hasExact && !explainExact {
		idmap.Set(string(exactID), 100, true)

		input := nearReportInput{
			QueryHashBase64: base64.StdEncoding.EncodeToString(fhash),
			QueryID:         base64.StdEncoding.EncodeToString(fhash),
			QueryType:       "external",
			QueryNames:      []string{filepath.Base(fpath)},
			ExactFileMatch:  true,
			ExplainExact:    false,
		}

		reportPath, err := writeNearJSONReportWithInput(input, idmap, deepEnabled, time.Since(start), db)
		if err != nil {
			return err
		}
		fmt.Printf("Exact file match found, skipping chunk drilldown.\n")
		fmt.Printf("NeAR report generated: %s\n", reportPath)
		return nil
	}

	var count int64
	echan := make(chan error)
	var active int
	for chonk := range getOutfileChonks(size, mappedFile) {
		if active >= cnst.GetMaxThreadCount() {
			workerErr := <-echan
			if workerErr != nil {
				return workerErr
			}
			active--
		}

		go processOutChunk(fhash, chonk, deepEnabled, idmap, db, echan)
		active++

		count++
	}

	for active > 0 {
		workerErr := <-echan
		if workerErr != nil {
			return workerErr
		}
		active--
	}

	err = updateConfidence(idmap, db)
	if err != nil {
		return err
	}

	input := nearReportInput{
		QueryHashBase64: base64.StdEncoding.EncodeToString(fhash),
		QueryID:         base64.StdEncoding.EncodeToString(fhash),
		QueryType:       "external",
		QueryNames:      []string{filepath.Base(fpath)},
		ExactFileMatch:  hasExact,
		ExplainExact:    explainExact,
	}

	reportPath, err := writeNearJSONReportWithInput(input, idmap, deepEnabled, time.Since(start), db)
	if err != nil {
		return err
	}
	fmt.Printf("NeAR report generated: %s\n", reportPath)
	fmt.Printf("\n\nNumber of chonks: %d", count)

	return nil
}

func getExactOutMatchID(fileHash []byte, db *badger.DB) ([]byte, bool, error) {
	candidates := [][]byte{
		util.AppendToBytesSlice(cnst.IdxFileNamespace, fileHash),
		util.AppendToBytesSlice(cnst.PartiFileNamespace, fileHash),
		util.AppendToBytesSlice(cnst.EviFileNamespace, fileHash),
	}

	for _, id := range candidates {
		err := dbio.PingNode(id, db)
		if err == nil {
			return id, true, nil
		}
		if err != badger.ErrKeyNotFound {
			return nil, false, err
		}
	}

	return nil, false, nil
}

func outfileSetup(fpath string) (int64, []byte, mmap.MMap, error) {
	finfo, err := os.Stat(fpath)
	if err != nil {
		return -1, nil, nil, err
	}
	fhandle, err := os.Open(fpath)
	if err != nil {
		return -1, nil, nil, err
	}

	fhash, err := util.GetFileHash(fhandle, cnst.GetHashAlgo(true))
	if err != nil {
		return -1, nil, nil, err
	}

	mappedFile, err := mmap.Map(fhandle, mmap.RDONLY, 0)
	if err != nil {
		return -1, nil, nil, err
	}

	return finfo.Size(), fhash, mappedFile, nil
}

func getOutfileChonks(size int64, mappedFile mmap.MMap) chan []byte {
	chonk := make(chan []byte)
	go func() {
		defer close(chonk)
		for outindex := int64(0); outindex <= size; outindex += cnst.ChonkSize {
			var buffSize int64
			if size-outindex <= cnst.ChonkSize {
				buffSize = size - outindex
			} else {
				buffSize = cnst.ChonkSize
			}
			chonk <- mappedFile[outindex : outindex+buffSize]
		}
	}()
	return chonk
}

func processOutChunk(fileHash, chonk []byte, deep bool, idmap *structs.ConcMap, db *badger.DB, echan chan error) {
	chash, err := util.GetChonkHash(chonk, cnst.GetHashAlgo())
	if err != nil {
		echan <- err
		return
	}

	nearGen, ok, err := buildNearGenForOutChunk(fileHash, chash, chonk, deep, db)
	if err != nil {
		echan <- err
		return
	}
	if !ok {
		echan <- nil
		return
	}

	countRList(fileHash, idmap, nearGen, db, echan)
}

func buildNearGenForOutChunk(fileHash, chash, chonk []byte, deep bool, db *badger.DB) (structs.NearGen, bool, error) {
	var nearGen structs.NearGen

	revmap, err := getOutChunkExactRevMap(chash, fileHash, db)
	if err != nil {
		return nearGen, false, err
	}
	if len(revmap) > 0 {
		nearGen.RevMap = revmap
		nearGen.Confidence = 1
		nearGen.MatchMethod = "shallow-exact"
		return nearGen, true, nil
	}

	if !deep {
		return nearGen, false, nil
	}

	inputSig := util.ChunkSimHash64(chonk)
	partialMatchKey, confidence, err := partialChonkMatch(fileHash, inputSig, db)
	if err != nil {
		return nearGen, false, err
	}
	if confidence < minDeepPartialConfidence || len(partialMatchKey) == 0 {
		return nearGen, false, nil
	}

	revmap, err = getOutChunkDeepRevMap(partialMatchKey, fileHash, db)
	if err != nil {
		return nearGen, false, err
	}
	if len(revmap) == 0 {
		return nearGen, false, nil
	}

	nearGen.RevMap = revmap
	nearGen.Confidence = confidence
	nearGen.MatchMethod = "deep-simhash"
	return nearGen, true, nil
}

func getOutChunkExactRevMap(chash, fileHash []byte, db *badger.DB) (map[int64][]string, error) {
	revkey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator, int64(0))
	similarMap, err := getRevRelSameChashPrefix(revkey, db)
	if err != nil {
		if err == badger.ErrKeyNotFound {
			return map[int64][]string{}, nil
		}
		return nil, err
	}

	out := make(map[int64][]string)
	for idx, revlist := range similarMap {
		filtered := make([]string, 0, len(revlist))
		for _, revhash := range revlist {
			if bytes.Equal([]byte(revhash), fileHash) {
				continue
			}
			filtered = append(filtered, revhash)
		}
		if len(filtered) > 0 {
			out[idx] = filtered
		}
	}

	return out, nil
}

func getOutChunkDeepRevMap(partialMatchKey, fileHash []byte, db *badger.DB) (map[int64][]string, error) {
	revmap, err := dbio.GetReverseRelationNode(partialMatchKey, db)
	if err != nil {
		return nil, err
	}

	idxSplit := bytes.Split(partialMatchKey, []byte(cnst.DataSeperator))
	idx, err := util.GetNumber(string(idxSplit[len(idxSplit)-1]))
	if err != nil {
		return nil, err
	}

	out := make(map[int64][]string)
	temp := make([]string, 0, len(revmap))
	for revid := range revmap {
		if bytes.Equal([]byte(revid), fileHash) {
			continue
		}
		temp = append(temp, revid)
	}
	if len(temp) > 0 {
		out[idx] = temp
	}

	similarMap, err := getRevRelSameChashPrefix(partialMatchKey, db)
	if err != nil {
		return nil, err
	}
	for tempIndex, tempRevList := range similarMap {
		filtered := make([]string, 0, len(tempRevList))
		for _, revhash := range tempRevList {
			if bytes.Equal([]byte(revhash), fileHash) {
				continue
			}
			filtered = append(filtered, revhash)
		}
		if len(filtered) > 0 {
			out[tempIndex] = filtered
		}
	}

	return out, nil
}
