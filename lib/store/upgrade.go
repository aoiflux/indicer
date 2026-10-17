package store

import (
	"context"
	"errors"
	"fmt"

	"indicer/lib/blobcodec"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/fio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

// maxDamagedSamples bounds the damaged records a BlockIndexUpgrade lists.
const maxDamagedSamples = 20

// BlockIndexUpgrade reports what UpgradeBlockIndex did.
type BlockIndexUpgrade struct {
	Files         int // block files read
	Records       int // records in them
	Migrated      int // records written to C|||
	AlreadyStored int // records whose chunk already had C||| metadata
	DamagedCount  int // records whose chunk could not be read or verified
	Damaged       []string
}

// Complete reports whether every block index record now has C||| metadata,
// so the block index is no longer needed.
func (u BlockIndexUpgrade) Complete() bool { return u.DamagedCount == 0 }

// UpgradeBlockIndex moves the chunk locations of a legacy hierarchical
// database's block index into C||| (D9). Each chunk is read from its
// container, decoded and checked against its hash before its location is
// written, so C||| never points at bytes that do not hold the chunk. A
// chunk that already has C||| metadata is left alone, which makes the
// upgrade safe to run again. Records that fail are counted in the report
// and keep only their block index entry. The block files are not changed.
//
// Errors are returned for what stops the upgrade: a damaged block file, a
// database error, or ctx ending.
func UpgradeBlockIndex(ctx context.Context, db *badger.DB) (BlockIndexUpgrade, error) {
	var report BlockIndexUpgrade
	files, err := fio.BlockIndexFiles(db.Opts().Dir)
	if err != nil {
		return report, err
	}
	report.Files = len(files)
	if len(files) == 0 {
		return report, nil
	}

	batch, err := util.InitBatch(db)
	if err != nil {
		return report, err
	}
	defer batch.Cancel()
	codec := blobcodec.For(db)
	hashLen := cnst.GetHashAlgo().Size()

	for _, path := range files {
		err := fio.ReadBlockFile(path, hashLen, func(r fio.BlockIndexRecord) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			report.Records++
			ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, r.Hash)
			_, err := dbio.GetNode(ckey, db)
			if err == nil {
				report.AlreadyStored++
				return nil
			}
			if !errors.Is(err, badger.ErrKeyNotFound) {
				return err
			}

			meta, err := verifiedContainerMeta(db.Opts().Dir, codec, ckey, r)
			if err != nil {
				report.DamagedCount++
				if len(report.Damaged) < maxDamagedSamples {
					report.Damaged = append(report.Damaged, fmt.Sprintf("%x: %v", r.Hash, err))
				}
				return nil
			}
			if err := dbio.SetBatchNode(ckey, meta, batch); err != nil {
				return err
			}
			report.Migrated++
			return nil
		})
		if err != nil {
			return report, fmt.Errorf("upgrade %s: %w", path, err)
		}
	}
	if err := batch.Flush(); err != nil {
		return report, err
	}
	return report, nil
}

// verifiedContainerMeta reads the chunk r locates in the database at dbDir,
// checks it decodes to content with the hash in ckey, and returns its encoded
// C||| metadata, with the container path in the relative form (STO-10).
func verifiedContainerMeta(dbDir string, codec blobcodec.Codec, ckey []byte, r fio.BlockIndexRecord) ([]byte, error) {
	path := dbio.ResolveBlobPath(dbDir, r.Path)
	stored, err := fio.ReadChunkFromContainer(path, r.Offset, r.Size)
	if err != nil {
		return nil, err
	}
	plain, err := codec.Decode(ckey, stored)
	if err != nil {
		return nil, err
	}
	if err := blobcodec.Verify(ckey, plain); err != nil {
		return nil, err
	}
	return msgpack.Marshal(structs.ChonkMetadata{
		Path:         dbio.StoredBlobPath(dbDir, path),
		Offset:       r.Offset,
		OriginalSize: int64(len(plain)),
		StoredSize:   r.Size,
		EncodedSize:  r.Size,
		Container:    true,
	})
}
