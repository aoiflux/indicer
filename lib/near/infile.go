package near

import (
	"bytes"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"strings"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/fatih/color"
	"github.com/schollz/progressbar/v3"
	"github.com/vmihailenco/msgpack/v5"
)

func NearInFile(fhash string, db *badger.DB, deep ...bool) error {
	fmt.Println("Finding NeAR artefacts & generating Artefact Relation Graph")
	start := time.Now()

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
		color.Red("DEEP option selected. NeAr calculation may take a long time.")
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

	err = visualise(fid, idmap, db)
	if err != nil {
		return err
	}

	fmt.Printf("Done.... %v\n", time.Since(start))
	return nil
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
		if active > cnst.GetMaxThreadCount() {
			if <-echan != nil {
				return nil, <-echan
			}
			bar.Add64(cnst.ChonkSize)
			active--
		}

		if near.Err != nil {
			return nil, near.Err
		}
		if len(near.RevMap) == 1 {
			continue
		}

		go countRList(fhash, idmap, near, db, echan)
		active++
	}

	for active > 0 {
		if <-echan != nil {
			return nil, <-echan
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

		chonks := float64(size / cnst.ChonkSize)
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

			if len(revmap) < 2 && deep {
				revmap, confidence, err = partialMatch(ehash, chash, db)
				if err != nil {
					neargen.Err = err
					neargenChan <- neargen
					return
				}
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
			neargenChan <- neargen
		}
	}()

	return neargenChan
}

func partialMatch(inhash, chash []byte, db *badger.DB) (map[string]struct{}, float64, error) {
	ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
	cdata, err := dbio.GetNode(ckey, db)
	if err != nil {
		return nil, float64(cnst.IgnoreVar), err
	}

	partialMatchKey, confidence, err := partialChonkMatch(inhash, cdata, db)
	if err != nil || confidence <= 0 {
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
