package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"indicer/lib/blobcodec"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/fio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// ErrMaterialMissing reports evidence whose relations, chunk locations or
// stored chunk bytes are not all present, so it cannot be restored and a
// FLUSHED ingest of it cannot be completed. Any other validation error is a
// failure to read, which says nothing about the evidence.
var ErrMaterialMissing = errors.New("ingest material missing")

// ErrMaterialCorrupt reports a stored chunk whose bytes do not decode to
// content with its hash: damaged, or not at the recorded place.
var ErrMaterialCorrupt = errors.New("stored chunk is corrupt")

// ValidationLevel selects how much ValidateEvidence checks.
type ValidationLevel int

const (
	// ValidateLocations checks that every chunk of the evidence has a
	// relation and a stored location (C||| or a legacy block index).
	ValidateLocations ValidationLevel = iota
	// ValidateMaterial also checks, by size, that each blob or container
	// holds the bytes its location records.
	ValidateMaterial
	// ValidateContent also reads each chunk, decodes it and checks it
	// against its hash.
	ValidateContent
)

// validateChunksPerTxn bounds how many chunks one validation transaction
// checks, so memory stays flat however large the evidence is.
const validateChunksPerTxn = 1024

// validatedCacheSize bounds the chunks one validation remembers as checked,
// so a chunk repeated across the evidence (zeros, mostly) is checked once
// per run of this many distinct chunks rather than at every occurrence.
const validatedCacheSize = 4096

// validateFlushed is the validation recovery and repair run; tests replace it.
var validateFlushed = ValidateFlushedEvidenceMaterialized

// ValidateFlushedEvidenceMaterialized checks that a FLUSHED ingest has every
// relation, chunk location and chunk byte it needs (ValidateMaterial), so
// recovery completes only evidence that can be restored. Missing or short
// material is ErrMaterialMissing (ING-10).
func ValidateFlushedEvidenceMaterialized(ctx context.Context, fileID []byte, evidenceFile structs.EvidenceFile, db *badger.DB) error {
	if evidenceFile.Completed {
		return nil
	}
	if evidenceFile.IngestState != structs.IngestStateFlushed {
		return fmt.Errorf("invalid state for flushed validation: %s", evidenceFile.IngestState)
	}
	return ValidateEvidence(ctx, db, fileID, evidenceFile, ValidateMaterial)
}

// ValidateEvidence checks the stored chunks of the evidence fileID at level,
// streaming its relations validateChunksPerTxn at a time. It returns nil for
// valid evidence, an error wrapping ErrMaterialMissing or ErrMaterialCorrupt
// for the first problem found, and any other error when reading failed.
//
// The chunk size is the database's, from its manifest; per-evidence chunk
// sizes arrive with the W6 evidence record.
func ValidateEvidence(ctx context.Context, db *badger.DB, fileID []byte, evidenceFile structs.EvidenceFile, level ValidationLevel) error {
	if evidenceFile.Size <= 0 {
		return nil
	}
	cs := dbmeta.ChunkSize(db)
	end := evidenceFile.Start + evidenceFile.Size
	checked := make(map[string]struct{}, validatedCacheSize)
	type ref struct {
		offset int64
		ckey   []byte
	}
	refs := make([]ref, 0, validateChunksPerTxn)

	for batchStart := (evidenceFile.Start / cs) * cs; batchStart < end; batchStart += validateChunksPerTxn * cs {
		if err := ctx.Err(); err != nil {
			return err
		}
		batchEnd := min(end, batchStart+validateChunksPerTxn*cs)
		refs = refs[:0]
		err := db.View(func(txn *badger.Txn) error {
			for offset := batchStart; offset < batchEnd; offset += cs {
				relKey := util.AppendToBytesSlice(cnst.RelationNamespace, fileID, cnst.DataSeperator, offset)
				item, err := txn.Get(relKey)
				if errors.Is(err, badger.ErrKeyNotFound) {
					return fmt.Errorf("%w: no relation at offset %d", ErrMaterialMissing, offset)
				}
				if err != nil {
					return fmt.Errorf("relation at offset %d: %w", offset, err)
				}
				chash, err := item.ValueCopy(nil)
				if err != nil {
					return fmt.Errorf("relation at offset %d: %w", offset, err)
				}
				// Relation values are zstd-compressed outside quick mode.
				if decoded, err := cnst.DECODER.DecodeAll(chash, nil); err == nil {
					chash = decoded
				}
				refs = append(refs, ref{offset, util.AppendToBytesSlice(cnst.ChonkNamespace, chash)})
			}
			return nil
		})
		if err != nil {
			return err
		}

		for _, r := range refs {
			if _, ok := checked[string(r.ckey)]; ok {
				continue
			}
			if err := validateChunk(db, r.ckey, level); err != nil {
				return fmt.Errorf("chunk %x at offset %d: %w", r.ckey[len(cnst.ChonkNamespace):], r.offset, err)
			}
			if len(checked) >= validatedCacheSize {
				clear(checked)
			}
			checked[string(r.ckey)] = struct{}{}
		}
	}
	return nil
}

// validateChunk checks one chunk at level.
func validateChunk(db *badger.DB, ckey []byte, level ValidationLevel) error {
	loc, err := dbio.LocateChunk(ckey, db)
	switch {
	case errors.Is(err, dbio.ErrChunkNotFound):
		return fmt.Errorf("%w: no stored location", ErrMaterialMissing)
	case errors.Is(err, dbio.ErrChunkMetaInvalid):
		return fmt.Errorf("%w: %v", ErrMaterialMissing, err)
	case err != nil:
		return fmt.Errorf("chunk location: %w", err)
	}
	if level == ValidateLocations {
		return nil
	}

	path := dbio.ResolveBlobPath(dbio.KVDir(db), loc.Path)
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s does not exist", ErrMaterialMissing, path)
	}
	if err != nil {
		return err
	}
	if loc.Container {
		if !fio.ContainerHolds(path, loc.Offset, loc.EncodedSize) {
			return fmt.Errorf("%w: %s ends before offset %d + %d", ErrMaterialMissing, path, loc.Offset, loc.EncodedSize)
		}
	} else if fi.Size() != loc.StoredSize {
		return fmt.Errorf("%w: %s holds %d bytes, its metadata records %d", ErrMaterialMissing, path, fi.Size(), loc.StoredSize)
	}
	if level == ValidateMaterial {
		return nil
	}

	plain, err := dbio.GetChonkNode(ckey, db)
	if errors.Is(err, blobcodec.ErrCorrupt) {
		return fmt.Errorf("%w: %v", ErrMaterialCorrupt, err)
	}
	if err != nil {
		return err
	}
	if err := blobcodec.Verify(ckey, plain); err != nil {
		return fmt.Errorf("%w: %v", ErrMaterialCorrupt, err)
	}
	return nil
}
