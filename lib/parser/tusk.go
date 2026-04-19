package parser

import (
	"encoding/json"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
)

// tuskResult is the top-level JSON returned by libtusk_analyze.
// libtusk may return either a partitioned layout (partitions[]) or a flat
// single-filesystem layout (filesystem + files at the top level).
type tuskResult struct {
	Image      string          `json:"image"`
	Partitions []tuskPartition `json:"partitions"`
	Filesystem *tuskFilesystem `json:"filesystem"`
	Files      []tuskFile      `json:"files"`
}

type tuskPartition struct {
	Name        string         `json:"name"`
	StartOffset int64          `json:"start_offset"`
	EndOffset   int64          `json:"end_offset"`
	Filesystem  tuskFilesystem `json:"filesystem"`
	Files       []tuskFile     `json:"files"`
}

type tuskFilesystem struct {
	Type      string `json:"type"`
	BlockSize int    `json:"block_size"`
	Offset    int64  `json:"offset"`
}

type tuskFile struct {
	Filename     string         `json:"filename"`
	Type         string         `json:"type"`
	IsFragmented bool           `json:"is_fragmented"`
	Size         int64          `json:"size"`
	Fragments    []tuskFragment `json:"fragments"`
}

type tuskFragment struct {
	StartOffset int64 `json:"start_offset"`
	EndOffset   int64 `json:"end_offset"`
}

// toPartitionFiles converts a tuskResult into the PartitionFile slice expected
// by the rest of the indexer. end_offset in libtusk output is inclusive.
//
// TODO: handle fragmented files — currently files with is_fragmented=true are
// skipped entirely. Fragmentation tracking needs to be lifted to the partition
// level before this can be supported properly.
func (r *tuskResult) toPartitionFiles() []structs.PartitionFile {
	if len(r.Partitions) > 0 {
		return r.partitionedLayout()
	}
	if r.Filesystem != nil {
		return r.flatLayout()
	}
	return nil
}

func (r *tuskResult) partitionedLayout() []structs.PartitionFile {
	plist := make([]structs.PartitionFile, 0, len(r.Partitions))
	for _, p := range r.Partitions {
		var pf structs.PartitionFile
		pf.Start = p.StartOffset
		pf.Size = p.EndOffset - p.StartOffset + 1
		plist = append(plist, pf)
	}
	return plist
}

func (r *tuskResult) flatLayout() []structs.PartitionFile {
	var pf structs.PartitionFile
	pf.Start = r.Filesystem.Offset

	return []structs.PartitionFile{pf}
}

// IndexFilesystem indexes all non-fragmented files described in the JSON
// output produced by libtusk_analyze. It mirrors IndexEXFAT but works from
// the tusk JSON instead of reading the filesystem directly.
func IndexFilesystem(jsonOutput string, pfile structs.InputFile, idxChan chan error) {
	var result tuskResult
	if err := json.Unmarshal([]byte(jsonOutput), &result); err != nil {
		idxChan <- err
		return
	}

	encodedPfileHash, err := pfile.GetEncodedHash()
	if err != nil {
		idxChan <- err
		return
	}

	batch, err := util.InitBatch(pfile.GetDB())
	if err != nil {
		idxChan <- err
		return
	}

	idxmap := make(map[string]structs.IndexedFile)
	var report indexReport
	if len(result.Partitions) > 0 {
		// Find the partition matching this pfile's start offset. Each goroutine
		// handles exactly one partition — processing all of them would be wrong
		// and calling tuskAnalyze per-goroutine would race the C library.
		var matched bool
		for i, p := range result.Partitions {
			if p.StartOffset != pfile.GetStartIndex() {
				continue
			}
			matched = true
			report.info(fmt.Sprintf("partition %d", i+1), fmt.Sprintf("%s (%s)", p.Name, p.Filesystem.Type))
			if n := countFragmented(p.Files); n > 0 {
				report.warning(fmt.Sprintf("partition %d", i+1), fmt.Sprintf("%d fragmented file(s) skipped (not supported yet)", n))
			}
			buildIdxMap(p.Files, idxmap, pfile, encodedPfileHash, idxChan)
			break
		}
		if !matched {
			idxChan <- cnst.ErrIncompatibleFile
			return
		}
	} else {
		fsType := ""
		if result.Filesystem != nil {
			fsType = result.Filesystem.Type
		}
		report.info("filesystem", fsType)
		if n := countFragmented(result.Files); n > 0 {
			report.warning("filesystem", fmt.Sprintf("%d fragmented file(s) skipped (not supported yet)", n))
		}
		buildIdxMap(result.Files, idxmap, pfile, encodedPfileHash, idxChan)
	}

	err = storeIndexedFiles(idxmap, pfile.GetDB(), batch, idxChan)
	if err != nil {
		idxChan <- err
		return
	}

	err = batch.Flush()
	if err != nil {
		idxChan <- err
		return
	}

	pchan := make(chan error)
	go store.Store(pfile, pchan)
	err = <-pchan
	report.print()
	idxChan <- err
}
