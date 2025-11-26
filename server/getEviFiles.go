package server

import (
	"context"
	"encoding/base64"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/pb"
	"log"

	"github.com/dgraph-io/badger/v4"
)

func (g *GrpcService) GetEviFiles(ctx context.Context, req *pb.GetEviFilesReq) (*pb.GetEviFilesRes, error) {
	var res pb.GetEviFilesRes

	// Get all evidence files from database
	eviFiles, err := getAllEvidenceFiles(cnst.DB)
	if err != nil {
		res.Err = err.Error()
		return &res, nil
	}

	// Convert to protobuf format
	for _, efile := range eviFiles {
		baseFile := &pb.BaseFile{
			FilePath: efile.Path,
			FileId:   efile.Hash,
			ChunkMap: make(map[string]int64),
		}
		res.EviFile = append(res.EviFile, baseFile)
	}

	res.Done = true
	log.Printf("GetEviFiles: returned %d files", len(eviFiles))
	return &res, nil
}

func getAllEvidenceFiles(db *badger.DB) ([]struct {
	Path string
	Hash string
}, error) {
	var files []struct {
		Path string
		Hash string
	}

	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(cnst.EviFileNamespace)
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			key := item.Key()

			efile, err := dbio.GetEvidenceFile(key, db)
			if err != nil {
				log.Printf("Error getting evidence file: %v", err)
				continue
			}

			// Extract hash bytes and encode to base64
			hashBytes := key[len(cnst.EviFileNamespace):]
			hashStr := base64.StdEncoding.EncodeToString(hashBytes)

			// Get first file path from names map
			for path := range efile.Names {
				files = append(files, struct {
					Path string
					Hash string
				}{
					Path: path,
					Hash: hashStr,
				})
				break // Just get one path per file
			}
		}
		return nil
	})

	return files, err
}
