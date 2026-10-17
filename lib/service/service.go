package service

import (
	"context"
	"encoding/base64"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/logging"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
)

// GetFileChunkMap returns the chunks that make up the bytes [fileStart,
// fileStart+fileSize) of the evidence whose raw ID is fileHash: each chunk's
// hash (base64) and the bytes it occupies in storage.
func GetFileChunkMap(fileStart, fileSize int64, fileHash []byte) (map[string]int64, error) {
	return chunkMap(context.Background(), cnst.DB, fileHash, fileStart, fileStart+fileSize)
}

func getEvidenceFile(filePath, fileHashStr string, db *badger.DB) (structs.EvidenceFile, error) {
	var efile structs.EvidenceFile
	fileHash, err := base64.StdEncoding.DecodeString(fileHashStr)
	if err != nil {
		logging.GetLogger().Error("Failed to decode file hash", zap.String("hash", fileHashStr), zap.Error(err))
		return efile, err
	}
	eid := util.AppendToBytesSlice(cnst.EviFileNamespace, fileHash)

	efile, err = dbio.GetEvidenceFile(eid, db)
	if err != nil {
		if err == badger.ErrKeyNotFound {
			logging.GetLogger().Warn("File not found in DB", zap.String("filepath", filePath))
			return efile, cnst.ErrFileNotFound
		}
		logging.GetLogger().Error("Failed to get evidence file", zap.String("filepath", filePath), zap.Error(err))
		return efile, err
	}
	if !efile.Completed {
		logging.GetLogger().Warn("File is incomplete", zap.String("filepath", filePath))
		return efile, cnst.ErrFileNotFound
	}
	return efile, nil
}

// chunkMap maps the hash of each chunk holding part of [start, end) of the
// evidence rid to the chunk's stored size, as its location records it. A
// chunk only partly in the range counts whole: it is stored whole.
func chunkMap(ctx context.Context, db *badger.DB, rid []byte, start, end int64) (map[string]int64, error) {
	ev, err := dbio.OpenEvidenceChunks(db, rid, dbio.ReadOptions{})
	if err != nil {
		return nil, err
	}
	m := make(map[string]int64)
	err = ev.Spans(ctx, start, end, dbio.SpanBatch(ev.ChunkSize()), func(spans []dbio.ChunkSpan) error {
		for _, s := range spans {
			m[base64.StdEncoding.EncodeToString(s.Hash)] = s.Loc.StoredSize
		}
		return nil
	})
	if err != nil {
		logging.GetLogger().Error("chunk map failed", zap.Int64("start", start), zap.Int64("end", end), zap.Error(err))
		return nil, err
	}
	return m, nil
}
