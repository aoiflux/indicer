package near

import (
	"bytes"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"time"

	"github.com/fatih/color"
	"github.com/schollz/progressbar/v3"
	"go.etcd.io/bbolt"
)

func NearInFile(fhash string, db *bbolt.DB, deep ...bool) error {
	fmt.Println("Finding NeAR artefacts & generating GReAt graph")
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

	err = visualise(fid, idmap, db)
	if err != nil {
		return err
	}

	fmt.Printf("Done.... %v\n", time.Since(start))
	return nil
}

func nearIndexFile(fid []byte, db *bbolt.DB, deep ...bool) (*structs.ConcMap, error) {
	ifile, err := dbio.GetIndexedFile(fid, db)
	if err != nil {
		return nil, err
	}
	var isdeep bool
	if len(deep) > 0 {
		isdeep = deep[0]
	}
	return getNearLogicalFile(ifile.Start, ifile.Size, ifile.Names[0], fid, db, isdeep)
}
func nearPartitionFile(fid []byte, db *bbolt.DB, deep ...bool) (*structs.ConcMap, error) {
	pfile, err := dbio.GetPartitionFile(fid, db)
	if err != nil {
		return nil, err
	}
	var isdeep bool
	if len(deep) > 0 {
		isdeep = deep[0]
	}
	return getNearLogicalFile(pfile.Start, pfile.Size, pfile.Names[0], fid, db, isdeep)
}
func nearEvidenceFile(fid []byte, db *bbolt.DB, deep ...bool) (*structs.ConcMap, error) {
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

func getNearLogicalFile(start, size int64, fname string, fid []byte, db *bbolt.DB, deep ...bool) (*structs.ConcMap, error) {
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
func getNearFile(start, size int64, ehash, fid []byte, db *bbolt.DB, deep ...bool) (*structs.ConcMap, error) {
	fhash := bytes.Split(fid, []byte(cnst.NamespaceSeperator))[1]
	idmap := structs.NewConcMap()
	rim := structs.NewRimMap()

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
		if len(near.RevList) == 1 {
			continue
		}

		go countRList(fhash, idmap, rim, near, db, echan)
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
	fmt.Println("Found NeAR Artefacts. Generating GReAt Graph....")
	return idmap, nil
}
func getNear(start, size int64, ehash []byte, db *bbolt.DB, deep ...bool) chan structs.NearGen {
	neargenChan := make(chan structs.NearGen)

	var dbstart int64
	if start > 0 {
		dbstart = util.GetDBStartOffset(start)
	}

	go func() {
		defer close(neargenChan)

		end := start + size
		var neargen structs.NearGen

		for nearIndex := dbstart; nearIndex <= end; nearIndex += cnst.ChonkSize {
			relKey := util.AppendToBytesSlice(cnst.RelationNamespace, ehash, cnst.DataSeperator, nearIndex)
			chash, err := dbio.GetNode(relKey, db)
			if err != nil {
				neargen.Err = err
				neargenChan <- neargen
			}

			revkey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash)
			revlist, err := dbio.GetReverseRelationNode(revkey, db)
			if err != nil {
				neargen.Err = err
				neargenChan <- neargen
			}
			if len(revlist) < 2 && len(deep) == 0 {
				continue
			}
			neargen.Confidence = 1

			if len(revlist) < 2 && deep[0] {
				ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
				cdata, err := dbio.GetNode(ckey, db)
				if err != nil {
					neargen.Err = err
					neargenChan <- neargen
				}

				pMatchKey, confidence, err := partialChonkMatch(cdata, db)
				if err != nil {
					neargen.Err = err
					neargenChan <- neargen
				}

				phash := bytes.Split(pMatchKey, []byte(cnst.NamespaceSeperator))[1]
				revkey = util.AppendToBytesSlice(cnst.ReverseRelationNamespace, phash)
				revlist, err = dbio.GetReverseRelationNode(revkey, db)
				if err != nil {
					neargen.Err = err
					neargenChan <- neargen
				}

				neargen.Confidence = confidence
			}

			neargen.RevList = revlist
			neargenChan <- neargen
		}
	}()

	return neargenChan
}
