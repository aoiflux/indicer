package near

import (
	"bytes"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"time"

	"github.com/dgraph-io/badger/v3"
	"github.com/schollz/progressbar/v3"
)

func NearInFile(fhash string, db *badger.DB) error {
	fmt.Println("Finding NeAR artefacts & generating GReAt graph")
	start := time.Now()

	fid, err := dbio.GuessFileType(fhash, db)
	if err != nil {
		return err
	}

	var idmap *structs.ConcMap

	if bytes.HasPrefix(fid, []byte(cnst.IdxFileNamespace)) {
		idmap, err = nearIndexFile(fid, db)
	} else if bytes.HasPrefix(fid, []byte(cnst.PartiFileNamespace)) {
		idmap, err = nearPartitionFile(fid, db)
	} else {
		idmap, err = nearEvidenceFile(fid, db)
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

func nearIndexFile(fid []byte, db *badger.DB) (*structs.ConcMap, error) {
	ifile, err := dbio.GetIndexedFile(fid, db)
	if err != nil {
		return nil, err
	}
	return getNearLogicalFile(ifile.Start, ifile.Size, ifile.Names[0], fid, db)
}
func nearPartitionFile(fid []byte, db *badger.DB) (*structs.ConcMap, error) {
	pfile, err := dbio.GetPartitionFile(fid, db)
	if err != nil {
		return nil, err
	}
	return getNearLogicalFile(pfile.Start, pfile.Size, pfile.Names[0], fid, db)
}
func nearEvidenceFile(fid []byte, db *badger.DB) (*structs.ConcMap, error) {
	efile, err := dbio.GetEvidenceFile(fid, db)
	if err != nil {
		return nil, err
	}
	ehash := bytes.Split(fid, []byte(cnst.NamespaceSeperator))[1]
	return getNearFile(efile.Start, efile.Size, ehash, fid, db)
}

func getNearLogicalFile(start, size int64, fname string, fid []byte, db *badger.DB) (*structs.ConcMap, error) {
	ehash, err := util.GetEvidenceFileHash(fname)
	if err != nil {
		return nil, err
	}
	return getNearFile(start, size, ehash, fid, db)
}
func getNearFile(start, size int64, ehash, fid []byte, db *badger.DB) (*structs.ConcMap, error) {
	fhash := bytes.Split(fid, []byte(cnst.NamespaceSeperator))[1]
	idmap := structs.NewConcMap()
	rim := structs.NewRimMap()

	fmt.Println("Finding NeAR Artefacts....")
	bar := progressbar.DefaultBytes(size)

	var active int
	echan := make(chan error)

	for near := range getNear(start, size, ehash, db) {
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

		go countRList(fhash, idmap, rim, near.RevList, db, echan)
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
func getNear(start, size int64, ehash []byte, db *badger.DB, deep ...bool) chan structs.NearGen {
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

			ckey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash)
			revlist, err := dbio.GetReverseRelationNode(ckey, db)
			if err != nil {
				neargen.Err = err
				neargenChan <- neargen
			}
			if len(revlist) < 2 {
				continue
			}

			neargen.RevList = revlist
			neargenChan <- neargen
		}
	}()

	return neargenChan
}
