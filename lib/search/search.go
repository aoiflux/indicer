package search

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/near"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/schollz/progressbar/v3"
)

var (
	cmap  *structs.SeenChonkMap
	idmap *structs.SearchIDMap
)

func init() {
	cmap = structs.NewSeenChonkMap()
	idmap = structs.NewSearchIDMap()
}

func Search(query string, db *badger.DB) error {
	start := time.Now()

	bar := progressbar.Default(100, "Searching....")

	re, err := regexp.Compile(query)
	if err != nil {
		return err
	}

	err = searchFiles(cnst.IdxFileNamespace, re, db)
	if err != nil {
		return err
	}
	bar.Add(20)

	err = searchFiles(cnst.PartiFileNamespace, re, db)
	if err != nil {
		return err
	}
	bar.Add(50)

	err = searchFiles(cnst.EviFileNamespace, re, db)
	if err != nil {
		return err
	}
	bar.Add(20)

	err = searchReport(query, db)
	if err != nil {
		return err
	}
	bar.Add(10)

	bar.Finish()
	fmt.Println("Done....", time.Since(start))
	return bar.Close()
}

func searchFiles(namespace string, re *regexp.Regexp, db *badger.DB) error {
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 1000
		it := txn.NewIterator(opts)
		defer it.Close()

		errChan := make(chan error)
		var active int

		prefix := []byte(namespace)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if active > cnst.GetMaxThreadCount() {
				active--
				err := <-errChan
				if err != nil {
					return err
				}
			}

			item := it.Item()
			fid := item.KeyCopy(nil)
			go searchAllFiles(re, fid, db, errChan)
			active++
		}

		for active > 0 {
			active--
			err := <-errChan
			if err != nil {
				return err
			}
		}
		return nil
	})

	return err
}

func searchAllFiles(re *regexp.Regexp, fid []byte, db *badger.DB, errChan chan error) {
	meta, err := store.GetFileMeta(fid, db)
	if err != nil {
		errChan <- err
	}
	errChan <- searchChonks(string(fid), re, meta, db)
}

func searchChonks(fidStr string, re *regexp.Regexp, meta structs.FileMeta, db *badger.DB) error {
	var dbstart int64
	if meta.Start > 0 {
		dbstart = util.GetDBStartOffset(meta.Start)
	}
	end := meta.Start + meta.Size

	echan := make(chan error)
	var active int
	for sindex := dbstart; sindex < end; sindex += cnst.ChonkSize {
		for active > cnst.GetMaxThreadCount() {
			active--
			err := <-echan
			if err != nil {
				return err
			}
		}
		go searchChonk(sindex, dbstart, end, fidStr, re, meta, db, echan)
		active++
	}

	for active > 0 {
		active--
		err := <-echan
		if err != nil {
			return err
		}
	}

	return nil
}

func searchChonk(sindex, dbstart, end int64, fid string, re *regexp.Regexp, meta structs.FileMeta, db *badger.DB, echan chan error) {
	nxtidx := sindex + cnst.ChonkSize
	if (nxtidx >= end) && (sindex != dbstart) {
		echan <- nil
		return
	}

	s1key, state1, err := getChonkState(sindex, dbstart, end, meta, db)
	if err != nil {
		echan <- err
		return
	}

	res, ok1 := cmap.Get(s1key)
	if (nxtidx >= end) && (sindex == dbstart) {
		if !ok1 {
			res = regexSearchChonk(sindex, re, state1)
			cmap.Set(s1key, res)
		}
		if res.Count > 0 {
			idmap.Set(fid, res)
		}
		echan <- nil
		return
	}

	s2key, state2, err := getChonkState(nxtidx, dbstart, end, meta, db)
	if err != nil {
		echan <- err
		return
	}

	res, ok2 := cmap.Get(s2key)
	if !ok1 || !ok2 {
		state := append(state1, state2...)
		res = regexSearchChonk(sindex, re, state)
	}
	if res.Count > 0 {
		idmap.Set(fid, res)
	}
	if !ok1 {
		cmap.Set(s1key, res)
	}
	if !ok2 {
		cmap.Set(s2key, res)
	}

	echan <- nil
}

func getChonkState(sindex, dbstart, end int64, meta structs.FileMeta, db *badger.DB) ([]byte, []byte, error) {
	relKey := util.AppendToBytesSlice(cnst.RelationNamespace, meta.EviHash, cnst.DataSeperator, sindex)
	chash, err := dbio.GetNode(relKey, db)
	if err != nil {
		return nil, nil, err
	}
	ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
	state, err := dbio.GetChonkData(sindex, meta.Start, meta.Size, dbstart, end, ckey, db)
	return ckey, state, err
}

func regexSearchChonk(sindex int64, re *regexp.Regexp, state []byte) structs.SearchResult {
	matches := re.FindAllIndex(state, -1)
	res := structs.SearchResult{Count: len(matches), Matches: make(map[int64]string)}

	for _, match := range matches {
		start := match[0]
		end := match[1]
		matchStr := string(state[start:end])

		matchStart := sindex + int64(start)
		res.Matches[matchStart] = matchStr
	}

	return res
}

func searchReport(query string, db *badger.DB) error {
	var report structs.SearchReport
	report.Query = query
	seenMap := make(map[string][]string)

	var occuranceCount, fileCount, artefactCount int
	artefactCount = len(idmap.GetData())

	for id, res := range idmap.GetData() {
		names, err := near.GetNames([]byte(id), db)
		if err != nil {
			return err
		}

		hash := strings.Split(id, cnst.NamespaceSeperator)[1]
		hashData := []byte(hash)
		hashStr := base64.StdEncoding.EncodeToString(hashData)

		var occurance structs.OccuranceData
		occurance.ArtefactHash = hashStr
		occurance.Count = res.Count
		for _, match := range res.Matches {
			if slices.Contains(occurance.Matches, match) {
				continue
			}
			occurance.Matches = append(occurance.Matches, match)
		}

		occurance.Disk = structs.NewDiskImage()
		err = setOccuranceData(occurance.ArtefactHash, names, seenMap, &occurance, db)
		if err != nil {
			return err
		}

		if occurance.Disk.Partition.Indexed != nil {
			if len(occurance.Disk.Partition.Indexed.IndexedFileNames) == 0 {
				occurance.Disk.Partition.Indexed = nil
			}
		}
		if occurance.Disk.Partition != nil {
			if len(occurance.Disk.Partition.PartitionPartNames) == 0 {
				occurance.Disk.Partition = nil
			}
		}
		if occurance.Disk != nil {
			if len(occurance.Disk.DiskImageNames) == 0 {
				occurance.Disk = nil
			}
		}

		fileCount += len(names)
		occuranceCount += res.Count
		report.Occurances = append(report.Occurances, occurance)
	}

	report.ExecutiveSummary = fmt.Sprintf("%d occurrences of the query `%s` were identified across %d digital artifacs(%d files) during a comprehensive electronic examination. This finding presents avenues for further forensic analysis and legal evaluation.", occuranceCount, query, artefactCount, fileCount)

	reportData, err := json.MarshalIndent(report, "", "\t")
	if err != nil {
		return err
	}

	return os.WriteFile("report.json", reportData, os.ModePerm)
}

func setOccuranceData(artefactHash string, names map[string]struct{}, smap map[string][]string, o *structs.OccuranceData, db *badger.DB) error {
	var idx int
	sameOccurance := false
	for name := range names {
		if idx > 1 {
			sameOccurance = true
		}

		split := strings.Split(name, cnst.DataSeperator)
		splitlen := len(split)

		switch splitlen {
		case 1:
			if len(o.FileNames) == 0 {
				o.FileNames = []string{name}
				continue
			}

			o.FileNames = append(o.FileNames, name)
			continue
		case 2:
			o.Disk.Partition.Indexed.IndexedFileHash = o.ArtefactHash
		case 3:
			o.Disk.Partition.PartitionHash = o.ArtefactHash
		default:
			errstr := fmt.Sprintf(cnst.ErrTooManySplits.Error(), name)
			return errors.New(errstr)
		}

		err := setDiskImageData(sameOccurance, artefactHash, split, smap, o, db)
		if err != nil {
			return err
		}

		idx++
	}
	return nil
}

func setDiskImageData(same bool, artefactHash string, nameSplit []string, smap map[string][]string, o *structs.OccuranceData, db *badger.DB) error {
	var err error

	splitlen := len(nameSplit)
	name := nameSplit[splitlen-1]

	switch splitlen {
	case 3:
		o.Disk.Partition.Indexed.IndexedFileHash = artefactHash
		if len(o.Disk.Partition.Indexed.IndexedFileNames) == 0 {
			o.Disk.Partition.Indexed.IndexedFileNames = []string{name}
		} else {
			o.Disk.Partition.Indexed.IndexedFileNames = append(o.Disk.Partition.Indexed.IndexedFileNames, name)
		}

		// set partition names
		fhashStr := nameSplit[1]
		o.Disk.Partition.PartitionHash = fhashStr
		val, ok := smap[cnst.PartiFileNamespace+fhashStr]
		if ok && !same {
			o.Disk.Partition.PartitionPartNames = val
		}
		if !ok {
			o.Disk.Partition.PartitionPartNames, err = getFileNames(cnst.PartiFileNamespace, fhashStr, db)
			if err != nil {
				return err
			}
			smap[cnst.PartiFileNamespace+fhashStr] = o.Disk.Partition.PartitionPartNames
		}

		// set disk image names
		fhashStr = nameSplit[0]
		o.Disk.DiskImageHash = fhashStr
		val, ok = smap[cnst.EviFileNamespace+fhashStr]
		if ok && !same {
			o.Disk.DiskImageNames = val
		}
		if !ok {
			o.Disk.DiskImageNames, err = getFileNames(cnst.EviFileNamespace, fhashStr, db)
			if err != nil {
				return err
			}
			smap[cnst.EviFileNamespace+fhashStr] = o.Disk.DiskImageNames
		}
	case 2:
		o.Disk.Partition.PartitionHash = artefactHash
		if len(o.Disk.Partition.PartitionPartNames) == 0 {
			o.Disk.Partition.PartitionPartNames = []string{name}
		} else {
			o.Disk.Partition.PartitionPartNames = append(o.Disk.Partition.PartitionPartNames, name)
		}

		fhashStr := nameSplit[0]
		o.Disk.DiskImageHash = fhashStr
		val, ok := smap[cnst.EviFileNamespace+fhashStr]
		if ok && !same {
			o.Disk.DiskImageNames = val
		}
		if !ok {
			o.Disk.DiskImageNames, err = getFileNames(cnst.EviFileNamespace, fhashStr, db)
			if err != nil {
				return err
			}
			smap[cnst.EviFileNamespace+fhashStr] = o.Disk.DiskImageNames
		}
	}

	return err
}

func getFileNames(namespace, fhashStr string, db *badger.DB) ([]string, error) {
	fhash, err := base64.StdEncoding.DecodeString(fhashStr)
	if err != nil {
		return nil, err
	}

	fid := util.AppendToBytesSlice(namespace, fhash)
	nameMap, err := near.GetNames(fid, db)
	if err != nil {
		return nil, err
	}

	var names []string
	for name := range nameMap {
		split := strings.Split(name, cnst.DataSeperator)
		name = split[len(split)-1]
		names = append(names, name)
	}

	return names, nil
}
